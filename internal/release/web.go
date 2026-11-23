package release

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const (
	webEntryLimit        = 10_000
	webFileLimit         = 64 << 20
	webTotalLimit        = 256 << 20
	webZIPDirectoryLimit = 8 << 20
)

type webDeployment struct {
	version string
	dir     string
	files   map[string]webFile
}

type webFile struct {
	info   fs.FileInfo
	sha256 string
}

type webPlan struct {
	files map[string]*zip.File
	dirs  map[string]bool
}

// WebHandler returns the optional static handler mounted at /. It only serves
// files indexed by a validated deployment.
func (s *Service) WebHandler() http.Handler {
	if !s.deployWeb {
		return nil
	}
	return http.HandlerFunc(s.serveWeb)
}

func (s *Service) serveWeb(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	state := s.state.Load()
	name, ok := strings.CutPrefix(r.URL.Path, "/")
	first, _, _ := strings.Cut(name, "/")
	if !ok || state == nil || state.web == nil || first == "v2" || first == "healthz" || first == "readyz" {
		http.NotFound(w, r)
		return
	}
	if name == "" || strings.HasSuffix(name, "/") {
		name += "index.html"
	}
	if !validWebPath(name) {
		http.NotFound(w, r)
		return
	}
	file, ok := state.web.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	dir, err := openDirectory(s.store, state.web.dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer dir.Close()
	f, st, err := openRegular(dir, filepath.FromSlash(name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	if !sameFile(file.info, st) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", `"`+file.sha256+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, path.Base(name), time.Time{}, f)
}

func (s *Service) deployLocalWeb(ctx context.Context) error {
	state := s.state.Load()
	var source *publication
	for _, p := range state.versions {
		if _, ok := p.manifest.Assets["web"]; ok && (source == nil || webNewer(p.manifest.Version, source.manifest.Version)) {
			source = p
		}
	}
	if source == nil || (state.web != nil && !webNewer(source.manifest.Version, state.web.version)) {
		return nil
	}
	stageName, stage, cleanup, err := newStage(s.store)
	if err != nil {
		return err
	}
	defer cleanup()
	deployment, err := s.installWeb(ctx, source, stageName, stage)
	if err != nil {
		return fmt.Errorf("deploy local Web package %q: %w", source.manifest.Version, err)
	}
	next := *state
	next.web = deployment
	s.state.Store(&next)
	return nil
}

func webNewer(a, b string) bool {
	if order, ok := compareVersions(a, b); ok && order != 0 {
		return order > 0
	}
	return a > b
}

func (s *Service) restoreWeb(ctx context.Context, state *releaseState) {
	for _, p := range state.versions {
		asset, ok := p.manifest.Assets["web"]
		if !ok || (state.web != nil && !webNewer(p.manifest.Version, state.web.version)) {
			continue
		}
		dest := webDestination(p, asset)
		dir, err := openDirectory(s.store, dest)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			var files map[string]webFile
			files, err = inspectDeployedWeb(ctx, p, asset, dir)
			dir.Close()
			if err == nil {
				state.web = &webDeployment{version: p.manifest.Version, dir: dest, files: files}
			}
		}
		if err != nil {
			s.restoreErr = errors.Join(s.restoreErr, fmt.Errorf("ignoring invalid Web deployment %q: %w", p.manifest.Version, err))
		}
	}
}

func webDestination(p *publication, asset Asset) string {
	return filepath.Join("web", p.manifest.Version+"-"+asset.SHA256)
}

func inspectDeployedWeb(ctx context.Context, p *publication, asset Asset, dir *os.Root) (map[string]webFile, error) {
	archive, plan, err := openWebZIP(p, asset)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	return verifyWebTree(ctx, dir, plan)
}

// Install Web files first. An unpaired Web directory left by a crash cannot be
// restored or served until its source package is also published and validated.
func (s *Service) installWeb(ctx context.Context, p *publication, stageName string, stage *os.Root) (*webDeployment, error) {
	asset, ok := p.manifest.Assets["web"]
	if !ok {
		return nil, nil
	}
	dest := webDestination(p, asset)
	dir, err := openDirectory(s.store, dest)
	if err == nil {
		defer dir.Close()
		files, err := inspectDeployedWeb(ctx, p, asset, dir)
		if err != nil {
			return nil, err
		}
		return &webDeployment{version: p.manifest.Version, dir: dest, files: files}, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	archive, plan, err := openWebZIP(p, asset)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	if err := stage.Mkdir("web", 0700); err != nil {
		return nil, err
	}
	dir, err = openDirectory(stage, "web")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	for name, entry := range plan.files {
		if err := dir.MkdirAll(filepath.FromSlash(path.Dir(name)), 0700); err != nil {
			return nil, err
		}
		if err := writeNewFile(dir, filepath.FromSlash(name), func(out *os.File) error {
			return copyZIPEntry(ctx, out, entry)
		}); err != nil {
			return nil, fmt.Errorf("extract Web file %q: %w", name, err)
		}
	}
	files, err := verifyWebTree(ctx, dir, plan)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := installDirectory(s.store, filepath.Join(stageName, "web"), dest); err != nil {
		return nil, err
	}
	return &webDeployment{version: p.manifest.Version, dir: dest, files: files}, nil
}

func openWebZIP(p *publication, asset Asset) (*os.File, webPlan, error) {
	if !strings.EqualFold(path.Ext(asset.Filename), ".zip") || asset.Bytes > maxAssetBytes {
		return nil, webPlan{}, errors.New("Web package must be a ZIP within the asset limit")
	}
	f, err := p.openAsset(asset)
	if err != nil {
		return nil, webPlan{}, err
	}
	if err := checkZIPDirectory(f, asset.Bytes); err != nil {
		f.Close()
		return nil, webPlan{}, err
	}
	archive, err := zip.NewReader(f, asset.Bytes)
	if err != nil {
		f.Close()
		return nil, webPlan{}, err
	}
	plan, err := planWeb(archive)
	if err != nil {
		f.Close()
		return nil, webPlan{}, err
	}
	return f, plan, nil
}

// The standard ZIP reader allocates from directory records before returning the
// file list. Check the classic, single-disk directory envelope first so neither
// a false record count nor a long sequence of headers can bypass our limits.
func checkZIPDirectory(r io.ReaderAt, size int64) error {
	const endLen, headerLen = 22, 46
	if size < endLen {
		return zip.ErrFormat
	}
	tail := make([]byte, min(size, endLen+65535))
	tailOffset := size - int64(len(tail))
	if _, err := r.ReadAt(tail, tailOffset); err != nil {
		return err
	}
	pos := bytes.LastIndex(tail[:len(tail)-endLen+4], []byte("PK\x05\x06"))
	if pos < 0 {
		return zip.ErrFormat
	}
	end := tail[pos:]
	le := binary.LittleEndian
	count := int(le.Uint16(end[10:12]))
	length := int64(le.Uint32(end[12:16]))
	offset := int64(le.Uint32(end[16:20]))
	if le.Uint32(end[4:8]) != 0 || int(le.Uint16(end[8:10])) != count ||
		count == 0 || count > webEntryLimit || length > webZIPDirectoryLimit ||
		offset+length != tailOffset+int64(pos) || len(end) != endLen+int(le.Uint16(end[20:22])) {
		return errors.New("invalid or oversized Web ZIP directory")
	}
	directory := bufio.NewReader(io.NewSectionReader(r, offset, length))
	var header [headerLen]byte
	for range count {
		if _, err := io.ReadFull(directory, header[:]); err != nil || string(header[:4]) != "PK\x01\x02" {
			return zip.ErrFormat
		}
		nameLen := int(le.Uint16(header[28:30]))
		extraLen := int(le.Uint16(header[30:32]))
		commentLen := int(le.Uint16(header[32:34]))
		if nameLen == 0 || nameLen > 513 || le.Uint16(header[34:36]) != 0 {
			return zip.ErrFormat
		}
		if _, err := directory.Discard(nameLen + extraLen + commentLen); err != nil {
			return zip.ErrFormat
		}
	}
	if _, err := directory.ReadByte(); !errors.Is(err, io.EOF) {
		return zip.ErrFormat
	}
	return nil
}

func validWebPath(name string) bool {
	if len(name) > 512 || strings.Count(name, "/") >= 20 || name == "." || !fs.ValidPath(name) ||
		strings.ContainsAny(name, `\:<>"|?*`) || strings.ContainsFunc(name, unicode.IsControl) {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if len(part) > 255 || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
	}
	_, err := filepath.Localize(name)
	return err == nil
}

func planWeb(archive *zip.Reader) (webPlan, error) {
	if len(archive.File) == 0 || len(archive.File) > webEntryLimit {
		return webPlan{}, errors.New("Web ZIP entry count is outside its limits")
	}
	type node struct {
		name string
		dir  bool
	}
	nodes := make(map[string]node)
	entries := make(map[string]bool)
	allFiles := make(map[string]*zip.File)
	var total uint64
	for _, z := range archive.File {
		name := strings.TrimSuffix(z.Name, "/")
		isDir := name != z.Name
		mode := z.Mode().Type()
		if !validWebPath(name) || (isDir && (mode != fs.ModeDir || z.UncompressedSize64 != 0)) || (!isDir && mode != 0) || entries[name] {
			return webPlan{}, fmt.Errorf("unsafe or duplicated Web ZIP entry %q", z.Name)
		}
		entries[name] = true
		for current, dir := name, isDir; current != "."; current, dir = path.Dir(current), true {
			key := strings.ToLower(current)
			next := node{name: current, dir: dir}
			if existing, ok := nodes[key]; ok && existing != next {
				return webPlan{}, fmt.Errorf("conflicting Web ZIP path %q", name)
			}
			nodes[key] = next
		}
		if isDir {
			continue
		}
		if z.UncompressedSize64 > webFileLimit || z.UncompressedSize64 > webTotalLimit-total {
			return webPlan{}, fmt.Errorf("expanded Web ZIP exceeds its limits at %q", name)
		}
		total += z.UncompressedSize64
		allFiles[name] = z
	}
	prefix := ""
	if allFiles["index.html"] == nil {
		// Accept a single wrapper directory, for example web/index.html.
		for name := range entries {
			first, _, _ := strings.Cut(name, "/")
			if prefix == "" {
				prefix = first + "/"
			} else if prefix != first+"/" {
				return webPlan{}, errors.New("Web ZIP must have a root index.html or one wrapper directory")
			}
		}
	}
	if index := allFiles[prefix+"index.html"]; index == nil || index.UncompressedSize64 == 0 {
		return webPlan{}, errors.New("Web ZIP requires a nonempty index.html")
	}
	plan := webPlan{files: make(map[string]*zip.File), dirs: map[string]bool{".": true}}
	for name, entry := range allFiles {
		normalized := strings.TrimPrefix(name, prefix)
		plan.files[normalized] = entry
		for dir := path.Dir(normalized); !plan.dirs[dir]; dir = path.Dir(dir) {
			plan.dirs[dir] = true
		}
	}
	return plan, nil
}

func copyZIPEntry(ctx context.Context, out io.Writer, z *zip.File) error {
	entry, err := z.Open()
	if err != nil {
		return err
	}
	defer entry.Close()
	limit := int64(z.UncompressedSize64)
	n, err := io.Copy(out, io.LimitReader(contextReader{ctx, entry}, limit+1))
	if err == nil && n != limit {
		err = errors.New("Web ZIP entry length differs from declared length")
	}
	return err
}

func verifyWebTree(ctx context.Context, dir *os.Root, plan webPlan) (map[string]webFile, error) {
	files := make(map[string]webFile, len(plan.files))
	err := fs.WalkDir(dir.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && plan.dirs[name] {
			return nil
		}
		z := plan.files[name]
		if z == nil || !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected entry in deployed Web tree %q", name)
		}
		want := sha256.New()
		if err := copyZIPEntry(ctx, want, z); err != nil {
			return err
		}
		asset := Asset{Filename: filepath.FromSlash(name), Bytes: int64(z.UncompressedSize64), SHA256: hex.EncodeToString(want.Sum(nil))}
		info, err := verifyAsset(ctx, dir, asset)
		if err != nil {
			return fmt.Errorf("deployed Web file %q differs from ZIP: %w", name, err)
		}
		files[name] = webFile{info: info, sha256: asset.SHA256}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) != len(plan.files) {
		return nil, errors.New("deployed Web tree is missing files")
	}
	return files, nil
}
