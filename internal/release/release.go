// Package release validates and serves local package releases and optional Web deployments.
package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"csac/v2/internal/rpc"
)

const (
	defaultPollInterval = 6 * time.Hour
	refreshTimeout      = 10 * time.Minute
	downloadPrefix      = "/v2/releases/"
)

var safeSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Manifest struct {
	Version     string           `json:"version"`
	Notes       string           `json:"notes"`
	PublishedAt time.Time        `json:"published_at"`
	Assets      map[string]Asset `json:"assets"`
}

type Asset struct {
	Filename string `json:"filename"`
	Download string `json:"download"`
	SHA256   string `json:"sha256"`
	Bytes    int64  `json:"bytes"`
}

// Options controls automatic ingestion and Web deployment. Both are off by default.
type Options struct {
	// GitHubRepo opts in to polling a public github.com OWNER/REPO.
	GitHubRepo string
	// PollInterval defaults to six hours when zero.
	PollInterval time.Duration
	// DeployWeb enables local deployment and serving of validated Web ZIP packages.
	DeployWeb bool
}

type publication struct {
	manifest Manifest
	root     *os.Root
	dir      string
	files    map[string]fs.FileInfo
}

type releaseState struct {
	latest   *publication
	versions map[string]*publication
	web      *webDeployment
}

// Service exposes snapshots of a validated local release store.
// Callers cancel and wait for Run before closing the service.
type Service struct {
	root       *os.Root
	store      *os.Root
	github     *githubSource
	interval   time.Duration
	deployWeb  bool
	restoreErr error
	state      atomic.Pointer[releaseState]
	syncMu     sync.Mutex
	closed     bool
}

// Open opens the local release store without automatic ingestion or Web serving.
func Open(dir string) (*Service, error) {
	return OpenWithOptions(dir, Options{})
}

// OpenWithOptions loads local releases. It performs no network requests.
func OpenWithOptions(dir string, opts Options) (*Service, error) {
	if opts.PollInterval < 0 {
		return nil, errors.New("release poll interval must be positive or zero for the default")
	}
	s := &Service{deployWeb: opts.DeployWeb, interval: opts.PollInterval}
	if s.interval == 0 {
		s.interval = defaultPollInterval
	}
	if opts.GitHubRepo != "" {
		var err error
		s.github, err = newGitHubSource(opts.GitHubRepo)
		if err != nil {
			return nil, err
		}
	}
	writable := s.github != nil || s.deployWeb
	if dir == "" {
		if writable {
			return nil, errors.New("automatic releases require a release root")
		}
		return s, nil
	}
	if writable {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("create release root: %w", err)
		}
	}
	root, err := openReleaseRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open release root: %w", err)
	}
	s.root = root
	ok := false
	defer func() {
		if !ok {
			s.Close()
		}
	}()

	state := &releaseState{versions: make(map[string]*publication)}
	local, err := loadPublication(context.Background(), root, ".", false)
	switch {
	case err == nil:
		state.latest = local
		state.versions[local.manifest.Version] = local
	case !errors.Is(err, errNoManifest):
		return nil, fmt.Errorf("load local release: %w", err)
	}
	s.store, err = openStore(root, writable)
	if err != nil {
		return nil, fmt.Errorf("open managed release store: %w", err)
	}
	if s.store != nil {
		if err := s.restore(state); err != nil {
			return nil, err
		}
	}
	s.state.Store(state)
	ok = true
	return s, nil
}

func (s *Service) Close() error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	for _, root := range []*os.Root{s.store, s.root} {
		if root != nil {
			errs = append(errs, root.Close())
		}
	}
	return errors.Join(errs...)
}

// Run refreshes immediately and then polls until ctx is canceled. With only Web
// deployment enabled, it performs a single refresh from local packages.
func (s *Service) Run(ctx context.Context, onError func(error)) {
	if s.restoreErr != nil && onError != nil && ctx.Err() == nil {
		onError(s.restoreErr)
	}
	for ctx.Err() == nil && (s.github != nil || s.deployWeb) {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
		if s.github == nil {
			return
		}
		timer := time.NewTimer(s.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// Sync performs one refresh. Requests continue to use the previous validated
// snapshot until the new release and any Web deployment have been committed.
func (s *Service) Sync(ctx context.Context) error {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	if s.closed {
		return fs.ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	var webErr error
	if s.deployWeb {
		webErr = s.deployLocalWeb(ctx)
	}
	if s.github == nil {
		return webErr
	}
	return errors.Join(webErr, s.ingestGitHub(ctx))
}

func (s *Service) Register(r *rpc.Registry) {
	r.Register("release.latest", rpc.Method{Access: rpc.Public, Handle: func(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
		in, err := rpc.Decode[struct {
			Version string `json:"installed_version"`
		}](raw)
		if err != nil {
			return nil, err
		}
		out := struct {
			Available bool      `json:"available"`
			Release   *Manifest `json:"release,omitempty"`
		}{}
		if state := s.state.Load(); state != nil && state.latest != nil {
			out.Release = &state.latest.manifest
			out.Available = newer(out.Release.Version, in.Version)
		}
		return out, nil
	}})
}

func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	state := s.state.Load()
	route, ok := strings.CutPrefix(r.URL.Path, downloadPrefix)
	version, platform, split := strings.Cut(route, "/")
	if state == nil || !ok || !split || !safeSegment.MatchString(version) || !safeSegment.MatchString(platform) {
		http.NotFound(w, r)
		return
	}
	p := state.versions[version]
	if p == nil {
		http.NotFound(w, r)
		return
	}
	a, ok := p.manifest.Assets[platform]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := p.openAsset(a)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Disposition", `attachment; filename="`+a.Filename+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+a.SHA256+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, a.Filename, p.manifest.PublishedAt, f)
}

func allowRead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "Method unavailable", http.StatusMethodNotAllowed)
	return false
}

func (s *Service) publish(p *publication, web *webDeployment) {
	previous := s.state.Load()
	next := &releaseState{latest: p, versions: maps.Clone(previous.versions), web: previous.web}
	next.versions[p.manifest.Version] = p
	if web != nil {
		next.web = web
	}
	s.state.Store(next)
}
