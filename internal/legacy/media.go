package legacy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"csac/v2/internal/media"

	"github.com/jackc/pgx/v5/pgtype"
)

type mediaRef struct {
	Table             string
	ID, Owner, Room   int64
	Position          int
	Scope, Path, Name string
	GroupMessage      bool
	ExpectedSize      int64
	Created           time.Time
	Expiry            *time.Time
}

type mediaItem struct {
	Ref                 mediaRef
	ID                  pgtype.UUID
	Purpose, MIME, Name string
	Size                int64
	Digest              [sha256.Size]byte
}

type mediaSource struct {
	root *os.Root
	opts Options
}

func openMediaSource(directory string, opts Options) (*mediaSource, error) {
	s := &mediaSource{opts: opts}
	if directory != "" {
		root, err := os.OpenRoot(directory)
		if err != nil {
			return nil, errors.New("cannot open legacy media root")
		}
		s.root = root
	}
	return s, nil
}

func (s *mediaSource) close() {
	if s.root != nil {
		s.root.Close()
	}
}

func relativeMediaPath(r mediaRef, origin string) (string, string) {
	if r.Path == "" {
		return "", "media_reference_empty"
	}
	u, err := url.Parse(r.Path)
	if err != nil || u.Opaque != "" || u.User != nil {
		return "", "media_path_rejected"
	}
	if u.Scheme != "" || u.Host != "" || strings.HasPrefix(r.Path, "//") {
		if origin == "" || u.Scheme+"://"+strings.ToLower(u.Host) != origin {
			return "", "media_external"
		}
	}
	p := strings.TrimPrefix(u.Path, "/")
	if !fs.ValidPath(p) || p == "." || len(p) > 1024 || strings.Count(p, "/") > 8 ||
		strings.Contains(p, "\\") || strings.IndexFunc(p, unicode.IsControl) >= 0 {
		return "", "media_path_rejected"
	}
	if r.Scope == "user-avatar" && !strings.Contains(p, "/") {
		// v1 stores some avatars as bare names, including its stock avatar.
		// Resolve them directly beneath upload/ without searching the archive.
		p = "upload/" + p
	}
	groupPrefix := "upload/files/groups/" + strconv.FormatInt(r.Room, 10) + "/"
	in := func(prefix string) bool { return strings.HasPrefix(p, prefix) }
	privateImage := in("uploads/chat/") && !in("uploads/chat/files/")
	valid := false
	switch r.Scope {
	case "user-avatar":
		valid = path.Dir(p) == "upload" || in("upload/avatar/") || in("upload/bot/")
	case "group-avatar":
		valid = in("upload/room/")
	case "message-image":
		// Older group uploads also appear directly under upload/.
		valid = in("upload/img/") || path.Dir(p) == "upload" || !r.GroupMessage && privateImage
	case "message-voice":
		valid = in("upload/voice/") || !r.GroupMessage && privateImage
	case "message-file":
		valid = r.GroupMessage && in(groupPrefix) || !r.GroupMessage && in("uploads/chat/files/")
	case "group-file":
		valid = in(groupPrefix)
	case "space-image":
		valid = in("upload/space_img/")
	case "sticker":
		valid = in("emojis/")
	}
	if !valid {
		return "", "media_path_rejected"
	}
	return p, ""
}

func (s *mediaSource) inspect(ctx context.Context, r mediaRef, copyTo io.Writer) (mediaItem, string, error) {
	if err := ctx.Err(); err != nil {
		return mediaItem{}, "", err
	}
	p, issue := relativeMediaPath(r, s.opts.MediaOrigin)
	if issue != "" {
		return mediaItem{}, issue, nil
	}
	if s.root == nil {
		return mediaItem{}, "media_root_unavailable", nil
	}
	// Reject even in-root symlinks. OpenRoot also confines the subsequent open
	// if a path component is changed during the check.
	var info fs.FileInfo
	parts := strings.Split(p, "/")
	for i := range parts {
		partInfo, err := s.root.Lstat(strings.Join(parts[:i+1], "/"))
		if errors.Is(err, fs.ErrNotExist) {
			return mediaItem{}, "media_missing", nil
		}
		if err != nil {
			return mediaItem{}, "", errors.New("cannot inspect a legacy media reference")
		}
		if i < len(parts)-1 && !partInfo.IsDir() || i == len(parts)-1 && !partInfo.Mode().IsRegular() {
			return mediaItem{}, "media_path_rejected", nil
		}
		info = partInfo
	}
	if info.Size() > s.opts.MaxMediaBytes {
		return mediaItem{}, "media_too_large", nil
	}
	if info.Size() == 0 || r.ExpectedSize < 0 || r.ExpectedSize > 0 && info.Size() != r.ExpectedSize {
		return mediaItem{}, "media_size_mismatch", nil
	}
	f, err := s.root.OpenFile(p, os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return mediaItem{}, "", errors.New("cannot read a legacy media reference")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) || info.Size() != opened.Size() {
		return mediaItem{}, "", errors.New("legacy media changed while opening a reference")
	}
	var header [512]byte
	n, err := io.ReadFull(f, header[:])
	if err != nil && err != io.ErrUnexpectedEOF {
		return mediaItem{}, "", errors.New("cannot read legacy media header")
	}
	name := r.Name
	if strings.TrimSpace(name) == "" {
		name = path.Base(p)
	}
	name = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(name))
	name = boundedUTF8(name, 255)
	if name == "" || name == "." || name == ".." {
		name = "attachment"
	}
	kind, _, _ := mime.ParseMediaType(http.DetectContentType(header[:n]))
	purpose := "message"
	switch r.Scope {
	case "user-avatar", "group-avatar":
		purpose = "avatar"
	case "space-image":
		purpose = "space"
	case "group-file":
		purpose = "group"
	}
	switch r.Scope {
	case "user-avatar", "group-avatar", "space-image", "message-image", "sticker":
		switch kind {
		case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "image/x-icon":
		default:
			return mediaItem{}, "media_invalid_image", nil
		}
	case "message-voice":
		switch kind {
		case "application/ogg":
			kind = "audio/ogg"
		case "video/webm":
			kind = "audio/webm"
		case "video/mp4":
			kind = "audio/mp4"
		default:
			if !strings.HasPrefix(kind, "audio/") {
				// The legacy service accepted containers that Go doesn't sniff.
				kind = "application/octet-stream"
			}
		}
	case "message-file", "group-file":
		// Preserve arbitrary file bytes and avoid serving them as active content.
		kind = "application/octet-stream"
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return mediaItem{}, "", errors.New("cannot rewind legacy media")
	}
	digest := sha256.New()
	var writer io.Writer = digest
	if copyTo != nil {
		writer = io.MultiWriter(digest, copyTo)
	}
	count, err := io.Copy(writer, io.LimitReader(contextReader{ctx: ctx, reader: f}, info.Size()+1))
	if err != nil || count != info.Size() {
		if canceled := ctx.Err(); canceled != nil {
			return mediaItem{}, "", canceled
		}
		return mediaItem{}, "", errors.New("cannot copy legacy media or its size changed")
	}
	after, err := f.Stat()
	if err != nil || !opened.ModTime().Equal(after.ModTime()) {
		return mediaItem{}, "", errors.New("legacy media changed while reading")
	}
	item := mediaItem{Ref: r, Purpose: purpose, MIME: kind, Name: name, Size: count}
	copy(item.Digest[:], digest.Sum(nil))
	// Include source identity and digest so a retry can reuse a durable file
	// left before a failed/unknown PostgreSQL commit. Distinct references get
	// distinct media IDs, respecting resource-level authorization.
	key, err := json.Marshal(item)
	if err != nil {
		return mediaItem{}, "", errors.New("cannot identify migrated media")
	}
	id := sha256.Sum256(append([]byte("csac-v1-media-v1\n"), key...))
	copy(item.ID.Bytes[:], id[:16])
	item.ID.Bytes[6] = item.ID.Bytes[6]&0x0f | 0x80
	item.ID.Bytes[8] = item.ID.Bytes[8]&0x3f | 0x80
	item.ID.Valid = true
	return item, "", nil
}

func (b *builder) attach(ref mediaRef) pgtype.UUID {
	if b.mediaErr != nil {
		return pgtype.UUID{}
	}
	item, reason, err := b.source.inspect(b.ctx, ref, nil)
	if err != nil {
		b.mediaErr = err
		return pgtype.UUID{}
	}
	if reason != "" {
		b.note(ref.Table, reason)
		return pgtype.UUID{}
	}
	b.media = append(b.media, item)
	return item.ID
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

type stagedMedia struct {
	root      *os.Root
	directory string
	published []string
}

func (p *Plan) stage(ctx context.Context, target string) (*stagedMedia, error) {
	if len(p.media) == 0 {
		return &stagedMedia{}, nil
	}
	if target == "" {
		return nil, errors.New("destination media root is required by the review")
	}
	if err := separateMediaRoots(p.sourceMedia, target); err != nil {
		return nil, err
	}
	if err := makeMediaVolume(target); err != nil {
		return nil, errors.New("cannot create destination media root")
	}
	root, err := os.OpenRoot(target)
	if err != nil {
		return nil, errors.New("cannot open destination media root")
	}
	tmp, err := os.MkdirTemp(target, ".csac-v1-stage-")
	if err != nil {
		root.Close()
		return nil, errors.New("cannot create destination media staging directory")
	}
	staged := &stagedMedia{root: root, directory: filepath.Base(tmp)}
	if err := p.fillStage(ctx, staged); err != nil {
		return nil, errors.Join(err, staged.close())
	}
	return staged, nil
}

func (p *Plan) fillStage(ctx context.Context, staged *stagedMedia) error {
	src, err := openMediaSource(p.sourceMedia, p.report.Options)
	if err != nil {
		return err
	}
	defer src.close()
	for _, m := range p.media {
		key := m.ID.String()
		f, err := staged.root.OpenFile(filepath.Join(staged.directory, key), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return errors.New("cannot create staged media file")
		}
		actual, reason, err := src.inspect(ctx, m.Ref, f)
		if err != nil || reason != "" || actual.ID != m.ID || actual.Digest != m.Digest || actual.Size != m.Size {
			f.Close()
			return errors.New("legacy media no longer matches the review")
		}
		err = errors.Join(f.Sync(), f.Close())
		if err != nil {
			return errors.New("cannot sync staged media file")
		}
	}
	return nil
}

func (s *stagedMedia) close() error {
	if s.root == nil {
		return nil
	}
	if err := errors.Join(s.root.RemoveAll(s.directory), s.root.Close()); err != nil {
		return errors.New("cannot clean destination media staging directory")
	}
	return nil
}

func (s *stagedMedia) publish(ctx context.Context, items []mediaItem, keys []string) error {
	dirs := make(map[string]bool)
	for i, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		id := item.ID.String()
		key, err := media.StorageKey(id)
		if err != nil || key != keys[i] {
			return errors.New("PostgreSQL media key disagrees with media service storage layout")
		}
		parts := strings.Split(key, "/")
		for i := 1; i < len(parts); i++ {
			dir := strings.Join(parts[:i], "/")
			if dirs[dir] {
				continue
			}
			if err := s.root.Mkdir(dir, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
				return errors.New("cannot create destination media shard")
			}
			stat, err := s.root.Lstat(dir)
			if err != nil || !stat.IsDir() {
				return errors.New("destination media shard must be a real directory")
			}
			dirs[dir] = true
		}
		err = s.root.Link(filepath.Join(s.directory, id), key)
		if errors.Is(err, fs.ErrExist) {
			if !s.matches(ctx, key, item) {
				return errors.New("destination media already contains a conflicting UUID file")
			}
		} else if err != nil {
			return errors.New("cannot publish destination media")
		} else {
			s.published = append(s.published, key)
		}
	}
	for dir := range dirs {
		if err := s.syncDir(dir); err != nil {
			return err
		}
	}
	return s.syncDir(".")
}

func (s *stagedMedia) matches(ctx context.Context, key string, item mediaItem) bool {
	info, err := s.root.Lstat(key)
	if err != nil || !info.Mode().IsRegular() || info.Size() != item.Size {
		return false
	}
	file, err := s.root.OpenFile(key, os.O_RDONLY|nonblockFlag, 0)
	if err != nil {
		return false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return false
	}
	digest := sha256.New()
	n, err := io.Copy(digest, io.LimitReader(contextReader{ctx: ctx, reader: file}, item.Size+1))
	return err == nil && n == item.Size && bytes.Equal(digest.Sum(nil), item.Digest[:])
}

func (s *stagedMedia) removePublished() error {
	var errs []error
	dirs := make(map[string]bool)
	for _, key := range s.published {
		if err := s.root.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, errors.New("cannot remove media published by the failed import"))
		}
		dirs[path.Dir(key)] = true
	}
	for dir := range dirs {
		errs = append(errs, s.syncDir(dir))
	}
	return errors.Join(errs...)
}

func (s *stagedMedia) syncDir(key string) error {
	if s.root == nil {
		return nil
	}
	dir, err := s.root.Open(key)
	if err != nil {
		return errors.New("cannot open destination media directory for sync")
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return errors.New("cannot sync destination media directory")
	}
	return nil
}

// Resolve the existing ancestor of a possibly new media volume before any
// write, so a destination alias cannot point back into the legacy tree.
func separateMediaRoots(source, destination string) error {
	src, err := filepath.EvalSymlinks(source)
	if err != nil {
		return errors.New("cannot resolve legacy media root")
	}
	src, err = filepath.Abs(src)
	if err != nil {
		return errors.New("cannot resolve legacy media root")
	}
	dst, err := filepath.Abs(destination)
	if err != nil {
		return errors.New("cannot resolve destination media root")
	}
	ancestor := dst
	for {
		real, err := filepath.EvalSymlinks(ancestor)
		if err == nil {
			suffix, err := filepath.Rel(ancestor, dst)
			if err != nil {
				return errors.New("cannot resolve destination media root")
			}
			dst = filepath.Join(real, suffix)
			break
		}
		if !errors.Is(err, fs.ErrNotExist) || filepath.Dir(ancestor) == ancestor {
			return errors.New("cannot resolve destination media root")
		}
		ancestor = filepath.Dir(ancestor)
	}
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
	}
	if contains(src, dst) || contains(dst, src) {
		return errors.New("legacy and destination media roots must be separate")
	}
	return nil
}

func makeMediaVolume(target string) error {
	abs, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	var parents []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			if err != nil {
				return err
			}
			break
		}
		parents = append(parents, filepath.Dir(dir))
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return err
	}
	for _, path := range parents {
		dir, err := os.Open(path)
		if err != nil {
			return err
		}
		if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
			return err
		}
	}
	return nil
}
