package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	githubAPIVersion   = "2022-11-28"
	githubMetadataMax  = 1 << 20
	githubAssetListMax = 256
)

var githubOwner = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]{0,38}$`)

type githubSource struct {
	baseURL            string
	allowAssetRedirect func(*url.URL) bool
}

type githubRelease struct {
	Tag         string        `json:"tag_name"`
	Notes       string        `json:"body"`
	PublishedAt time.Time     `json:"published_at"`
	Draft       bool          `json:"draft"`
	Prerelease  bool          `json:"prerelease"`
	Assets      []githubAsset `json:"assets"`
}

type githubAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	State  string `json:"state"`
}

func newGitHubSource(repo string) (*githubSource, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !githubOwner.MatchString(owner) || !safeSegment.MatchString(name) || strings.HasSuffix(strings.ToLower(name), ".git") {
		return nil, errors.New("GitHub repository must be a public OWNER/REPO name")
	}
	return &githubSource{
		baseURL: "https://api.github.com/repos/" + owner + "/" + name,
		allowAssetRedirect: func(u *url.URL) bool {
			if u.Scheme != "https" || u.User != nil {
				return false
			}
			switch strings.ToLower(u.Host) {
			case "release-assets.githubusercontent.com", "objects.githubusercontent.com", "github-releases.githubusercontent.com":
				return true
			default:
				return false
			}
		},
	}, nil
}

func (g *githubSource) get(ctx context.Context, endpoint, accept string, asset bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.baseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", "CsAC-v2-releases")
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !asset || len(via) >= 5 || !g.allowAssetRedirect(req.URL) {
				return errors.New("GitHub redirect rejected")
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		// A redirect URL may contain a signed CDN query. Keep it out of logs.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return nil, uerr.Err
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		resp.Body.Close()
		return nil, errors.New("unexpected GitHub content encoding")
	}
	return resp, nil
}

func (g *githubSource) latest(ctx context.Context) (githubRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var release githubRelease
	resp, err := g.get(ctx, "/releases/latest", "application/vnd.github+json", false)
	if err != nil {
		return release, err
	}
	defer resp.Body.Close()
	data, err := readBounded(resp.Body, githubMetadataMax)
	if err != nil {
		return release, err
	}
	return release, json.Unmarshal(data, &release)
}

func (g *githubSource) download(ctx context.Context, dir *os.Root, asset githubAsset) (Asset, error) {
	out := Asset{Filename: asset.Name, Bytes: asset.Size}
	resp, err := g.get(ctx, "/releases/assets/"+strconv.FormatInt(asset.ID, 10), "application/octet-stream", true)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.ContentLength >= 0 && resp.ContentLength != asset.Size {
		return out, errors.New("Content-Length differs from reported size")
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
		return out, errors.New("expected asset bytes, received JSON")
	}
	err = writeNewFile(dir, asset.Name, func(f *os.File) error {
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, asset.Size+1))
		if err != nil {
			return err
		}
		if n != asset.Size {
			return errors.New("download size differs from reported size")
		}
		out.SHA256 = hex.EncodeToString(h.Sum(nil))
		if asset.Digest != "" && !strings.EqualFold(asset.Digest, "sha256:"+out.SHA256) {
			return errors.New("download SHA-256 differs from reported digest")
		}
		return nil
	})
	return out, err
}

func selectAssets(all []githubAsset) (map[string]githubAsset, error) {
	if len(all) > githubAssetListMax {
		return nil, errors.New("too many GitHub assets")
	}
	assets := make(map[string]githubAsset)
	filenames := make(map[string]bool)
	ids := make(map[int64]bool)
	var total int64
	for _, a := range all {
		platform := assetPlatform(a.Name)
		if platform == "" {
			continue
		}
		lowerName := strings.ToLower(a.Name)
		if !safeSegment.MatchString(a.Name) || lowerName == manifestName || a.ID < 1 || a.State != "uploaded" || a.Size < 1 {
			return nil, fmt.Errorf("invalid GitHub asset %q", a.Name)
		}
		if a.Digest != "" {
			digest, ok := strings.CutPrefix(a.Digest, "sha256:")
			if !ok || !validSHA256(digest) {
				return nil, fmt.Errorf("invalid SHA-256 digest for GitHub asset %q", a.Name)
			}
		}
		if _, exists := assets[platform]; exists || filenames[lowerName] || ids[a.ID] {
			return nil, fmt.Errorf("ambiguous GitHub asset for platform %q", platform)
		}
		if len(assets) >= maxAssets || a.Size > maxAssetBytes || a.Size > maxTotalBytes-total {
			return nil, fmt.Errorf("GitHub asset %q exceeds release limits", a.Name)
		}
		assets[platform] = a
		filenames[lowerName], ids[a.ID] = true, true
		total += a.Size
	}
	if len(assets) == 0 {
		return nil, errors.New("no supported packages in GitHub release")
	}
	return assets, nil
}

func assetPlatform(name string) string {
	name = strings.ToLower(name)
	if rest, ok := strings.CutPrefix(name, "csacterminal"); ok {
		name = rest
	} else {
		name = strings.TrimPrefix(name, "csac")
	}
	ext := path.Ext(name)
	switch ext {
	case "", ".zip", ".gz", ".xz", ".bz2", ".zst", ".tar", ".apk", ".aab", ".ipa", ".exe", ".msi", ".msix", ".msixbundle", ".dmg", ".pkg", ".deb", ".rpm", ".appimage":
	default:
		// Some standalone executables have no extension and end in a version.
		if len(ext) < 2 || ext[1] < '0' || ext[1] > '9' {
			return ""
		}
		ext = ""
	}
	words := strings.FieldsFunc(strings.TrimSuffix(name, ext), func(r rune) bool {
		return r < 'a' || r > 'z'
	})
	has := func(word string) bool { return slices.Contains(words, word) }
	var native string
	switch {
	case has("windows"), has("win"), ext == ".exe", ext == ".msi", ext == ".msix", ext == ".msixbundle":
		native = "windows"
	case has("linux"), ext == ".deb", ext == ".rpm", ext == ".appimage":
		native = "linux"
	case has("macos"), has("darwin"), has("osx"), has("mac"), ext == ".dmg", ext == ".pkg":
		native = "macos"
	}
	switch {
	case ext == ".zip" && has("web"):
		return "web"
	case has("serverbot"), has("bot"):
		if native == "linux" {
			return "bot-linux"
		}
		return "bot"
	case has("acop"):
		if native == "linux" {
			return "acop-server-linux"
		}
		return "acop-server"
	case has("server"), has("rpcserver"):
		if native != "" {
			return "server-" + native
		}
		return "server"
	case has("android"), ext == ".apk", ext == ".aab":
		return "android"
	case has("ios"), ext == ".ipa":
		return "ios"
	default:
		return native
	}
}

func (s *Service) ingestGitHub(ctx context.Context) error {
	release, err := s.github.latest(ctx)
	if err != nil {
		return fmt.Errorf("check GitHub release: %w", err)
	}
	if release.Draft || release.Prerelease {
		return errors.New("GitHub latest release is a draft or marked prerelease")
	}
	if _, _, ok := parseVersion(release.Tag); !ok || release.PublishedAt.IsZero() {
		return errors.New("GitHub release requires a CsAC version tag and publication time")
	}
	version := stripVersionPrefix(release.Tag)
	state := s.state.Load()
	if state.latest != nil {
		order, ok := compareVersions(version, state.latest.manifest.Version)
		if !ok {
			return errors.New("cannot compare GitHub tag with the active local release version")
		}
		if order <= 0 {
			return nil
		}
	}
	assets, err := selectAssets(release.Assets)
	if err != nil {
		return err
	}
	manifest := Manifest{Version: version, Notes: release.Notes, PublishedAt: release.PublishedAt, Assets: make(map[string]Asset)}
	// Reject oversized release notes before downloading packages.
	if len(release.Notes) >= manifestLimit {
		return errors.New("GitHub release notes exceed the manifest limit")
	}
	stageName, stage, cleanup, err := newStage(s.store)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := stage.Mkdir("release", 0700); err != nil {
		return err
	}
	packages, err := openDirectory(stage, "release")
	if err != nil {
		return err
	}
	defer packages.Close()
	for platform, a := range assets {
		file, err := s.github.download(ctx, packages, a)
		if err != nil {
			return fmt.Errorf("download %s package %q: %w", platform, a.Name, err)
		}
		manifest.Assets[platform] = file
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || len(encoded) > manifestLimit {
		return errors.New("GitHub release manifest exceeds the local manifest limit")
	}
	if err := writeBytes(packages, manifestName, encoded); err != nil {
		return err
	}
	p, err := loadPublication(ctx, stage, "release", true)
	if err != nil {
		return fmt.Errorf("validate staged release: %w", err)
	}
	var web *webDeployment
	if s.deployWeb {
		web, err = s.installWeb(ctx, p, stageName, stage)
		if err != nil {
			return fmt.Errorf("deploy Web package: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dest := filepath.Join("releases", version)
	if err := installDirectory(s.store, filepath.Join(stageName, "release"), dest); err != nil {
		return fmt.Errorf("publish %q: %w", version, err)
	}
	p.root, p.dir = s.store, dest
	s.publish(p, web)
	return nil
}
