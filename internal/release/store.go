package release

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	managedDir    = ".csac-releases"
	storeOwner    = "csac-release-store-v1\n"
	manifestName  = "manifest.json"
	manifestLimit = 64 << 10
	maxAssets     = 32
	maxAssetBytes = 512 << 20
	maxTotalBytes = 1 << 30
)

var errNoManifest = errors.New("missing release manifest")

func openReleaseRoot(path string) (*os.Root, error) {
	path = filepath.Clean(path)
	return checkedDirectory(path, os.Lstat, os.OpenRoot)
}

func openDirectory(root *os.Root, name string) (*os.Root, error) {
	return checkedDirectory(name, root.Lstat, root.OpenRoot)
}

func checkedDirectory(name string, lstat func(string) (fs.FileInfo, error), open func(string) (*os.Root, error)) (*os.Root, error) {
	st, err := lstat(name)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%q is not a real directory", name)
	}
	root, err := open(name)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(st, actual) {
		root.Close()
		return nil, fmt.Errorf("directory %q changed while opening", name)
	}
	return root, nil
}

func openRegular(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	st, err := root.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%q is not a regular file", name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	actual, err := f.Stat()
	if err != nil || !sameFile(st, actual) {
		f.Close()
		return nil, nil, fmt.Errorf("file %q changed while opening", name)
	}
	return f, actual, nil
}

func sameFile(want, got fs.FileInfo) bool {
	return want != nil && got != nil && got.Mode().IsRegular() && os.SameFile(want, got) &&
		want.Size() == got.Size() && want.ModTime().Equal(got.ModTime())
}

func openStore(root *os.Root, create bool) (*os.Root, error) {
	store, err := openDirectory(root, managedDir)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, nil
		}
		if err := initializeStore(root); err != nil {
			return nil, err
		}
		store, err = openDirectory(root, managedDir)
	}
	if err != nil {
		return nil, err
	}
	if err := verifyStore(store); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func initializeStore(root *os.Root) error {
	stageName, stage, cleanup, err := newStage(root)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := writeBytes(stage, "owner", []byte(storeOwner)); err != nil {
		return err
	}
	for _, name := range []string{"releases", "web"} {
		if err := stage.Mkdir(name, 0700); err != nil {
			return err
		}
	}
	return installDirectory(root, stageName, managedDir)
}

func verifyStore(store *os.Root) error {
	marker, _, err := openRegular(store, "owner")
	if err != nil {
		return fmt.Errorf("missing or invalid %s ownership marker: %w", managedDir, err)
	}
	defer marker.Close()
	contents, err := io.ReadAll(io.LimitReader(marker, int64(len(storeOwner)+1)))
	if err != nil || string(contents) != storeOwner {
		return fmt.Errorf("invalid %s ownership marker", managedDir)
	}
	for _, name := range []string{"releases", "web"} {
		dir, err := openDirectory(store, name)
		if err != nil {
			return err
		}
		dir.Close()
	}
	return nil
}

func newStage(root *os.Root) (string, *os.Root, func(), error) {
	name := ".stage-" + rand.Text()
	if err := root.Mkdir(name, 0700); err != nil {
		return "", nil, nil, err
	}
	stage, err := openDirectory(root, name)
	if err != nil {
		root.RemoveAll(name)
		return "", nil, nil, err
	}
	return name, stage, func() {
		stage.Close()
		root.RemoveAll(name)
	}, nil
}

// Only rename an owned stage into a previously unused path. Nonempty committed
// directories also prevent POSIX rename from replacing another publication.
func installDirectory(root *os.Root, stage, dest string) error {
	if _, err := root.Lstat(dest); !errors.Is(err, fs.ErrNotExist) {
		if err != nil {
			return err
		}
		return fmt.Errorf("destination %q already exists", dest)
	}
	return root.Rename(stage, dest)
}

func writeBytes(root *os.Root, name string, content []byte) error {
	return writeNewFile(root, name, func(f *os.File) error {
		_, err := f.Write(content)
		return err
	})
}

func writeNewFile(root *os.Root, name string, write func(*os.File) error) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0400)
	if err != nil {
		return err
	}
	err = write(f)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

func loadPublication(ctx context.Context, root *os.Root, rel string, managed bool) (*publication, error) {
	dir, err := openDirectory(root, rel)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	manifest, err := readManifest(dir)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{manifestName: true}
	var total int64
	for platform, a := range manifest.Assets {
		if !safeSegment.MatchString(platform) || !safeSegment.MatchString(a.Filename) || strings.EqualFold(a.Filename, manifestName) || a.Bytes < 1 || !validSHA256(a.SHA256) {
			return nil, fmt.Errorf("invalid release asset for platform %q", platform)
		}
		if managed && (names[a.Filename] || a.Bytes > maxAssetBytes || a.Bytes > maxTotalBytes-total) {
			return nil, fmt.Errorf("invalid or oversized managed asset %q", a.Filename)
		}
		if managed {
			total += a.Bytes
		}
		names[a.Filename] = true
		a.SHA256 = strings.ToLower(a.SHA256)
		a.Download = downloadPrefix + manifest.Version + "/" + platform
		manifest.Assets[platform] = a
	}
	if managed {
		if _, _, ok := parseVersion(manifest.Version); !ok {
			return nil, errors.New("invalid managed version")
		}
		entries, err := fs.ReadDir(dir.FS(), ".")
		if err != nil {
			return nil, err
		}
		if len(entries) != len(names) {
			return nil, errors.New("managed release files differ from manifest")
		}
		for _, entry := range entries {
			if !names[entry.Name()] || !entry.Type().IsRegular() {
				return nil, fmt.Errorf("unexpected managed release entry %q", entry.Name())
			}
		}
	}
	files := make(map[string]fs.FileInfo, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		st, err := verifyAsset(ctx, dir, asset)
		if err != nil {
			return nil, fmt.Errorf("validate %q: %w", asset.Filename, err)
		}
		files[asset.Filename] = st
	}
	return &publication{manifest: manifest, root: root, dir: rel, files: files}, nil
}

func readManifest(dir *os.Root) (Manifest, error) {
	var manifest Manifest
	f, _, err := openRegular(dir, manifestName)
	if errors.Is(err, fs.ErrNotExist) {
		return manifest, errNoManifest
	}
	if err != nil {
		return manifest, err
	}
	defer f.Close()
	data, err := readBounded(f, manifestLimit)
	if err != nil {
		return manifest, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return manifest, errors.New("expected exactly one release manifest")
	}
	if !safeSegment.MatchString(manifest.Version) || len(manifest.Assets) == 0 || len(manifest.Assets) > maxAssets {
		return manifest, errors.New("invalid release manifest")
	}
	return manifest, nil
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	var digest [sha256.Size]byte
	_, err := hex.Decode(digest[:], []byte(value))
	return err == nil
}

func verifyAsset(ctx context.Context, root *os.Root, asset Asset) (fs.FileInfo, error) {
	f, st, err := openRegular(root, asset.Filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st.Size() != asset.Bytes {
		return nil, errors.New("size differs from manifest")
	}
	h := sha256.New()
	// Limit to the stat'ed size and check both the resulting hash and final stat.
	n, err := io.Copy(h, io.LimitReader(contextReader{ctx, f}, asset.Bytes))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !sameFile(st, after) || n != asset.Bytes || hex.EncodeToString(h.Sum(nil)) != asset.SHA256 {
		return nil, errors.New("size or SHA-256 differs from manifest")
	}
	return after, nil
}

func (p *publication) openAsset(asset Asset) (*os.File, error) {
	dir, err := openDirectory(p.root, p.dir)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, st, err := openRegular(dir, asset.Filename)
	if err != nil {
		return nil, err
	}
	if !sameFile(p.files[asset.Filename], st) {
		f.Close()
		return nil, errors.New("validated asset changed")
	}
	return f, nil
}

func (s *Service) restore(state *releaseState) error {
	entries, err := fs.ReadDir(s.store.FS(), "releases")
	if err != nil {
		return fmt.Errorf("read committed releases: %w", err)
	}
	ctx := context.Background()
	for _, entry := range entries {
		version := entry.Name()
		if _, _, ok := parseVersion(version); !ok || !entry.IsDir() {
			s.restoreErr = errors.Join(s.restoreErr, fmt.Errorf("ignoring unexpected managed release %q", version))
			continue
		}
		p, err := loadPublication(ctx, s.store, filepath.Join("releases", version), true)
		if err != nil {
			s.restoreErr = errors.Join(s.restoreErr, fmt.Errorf("ignoring invalid managed release %q: %w", version, err))
			continue
		}
		if p.manifest.Version != version || state.versions[version] != nil {
			s.restoreErr = errors.Join(s.restoreErr, fmt.Errorf("ignoring mismatched or duplicate managed version %q", version))
			continue
		}
		state.versions[version] = p
		if state.latest == nil || newer(version, state.latest.manifest.Version) {
			state.latest = p
		}
	}
	if s.deployWeb {
		s.restoreWeb(ctx, state)
	}
	return nil
}

func readBounded(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err == nil && int64(len(data)) > maxBytes {
		err = fmt.Errorf("response or file exceeds %d bytes", maxBytes)
	}
	return data, err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
