package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	objectsDir      = "objects"
	stagingDir      = "staging"
	maxImportBytes  = 256 << 20
	staleStagingAge = time.Hour
)

// StorageKey returns the same normalized locator as media_objects.stored_key.
func StorageKey(id string) (string, error) {
	if !ValidID(id) {
		return "", errors.New("invalid media storage ID")
	}
	return objectsDir + "/" + id[:2] + "/" + id[2:4] + "/" + id, nil
}

func openStore(location string) (*os.Root, error) {
	if location == "" {
		return nil, errors.New("media root is required")
	}
	if err := createVolume(location); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(location)
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{objectsDir, stagingDir} {
		if err := realDirectory(root, dir, true); err != nil {
			_ = root.Close()
			return nil, err
		}
	}
	return root, nil
}

// Persist any directory entries created for the configured volume itself.
func createVolume(location string) error {
	abs, err := filepath.Abs(location)
	if err != nil {
		return err
	}
	var parents []string
	for dir := abs; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
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
	for _, dir := range parents {
		parent, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = parent.Sync()
		_ = parent.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func realDirectory(root *os.Root, name string, create bool) error {
	if create {
		if err := root.Mkdir(name, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("media storage requires real directories")
	}
	return nil
}

func objectDirectories(root *os.Root, key string, create bool) error {
	leaf := path.Dir(key)
	for _, dir := range []string{objectsDir, path.Dir(leaf), leaf} {
		if err := realDirectory(root, dir, create); err != nil {
			return err
		}
	}
	return nil
}

func newID() string {
	var id pgtype.UUID
	_, _ = rand.Read(id.Bytes[:])
	id.Bytes[6] = (id.Bytes[6] & 0x0f) | 0x40
	id.Bytes[8] = (id.Bytes[8] & 0x3f) | 0x80
	id.Valid = true
	return id.String()
}

type staged struct {
	name     string
	size     int64
	detected string
}

func stage(ctx context.Context, root *os.Root, reader io.Reader, limit int64) (staged, error) {
	name := filepath.Join(stagingDir, newID()+".part")
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return staged{}, err
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = root.Remove(name)
		}
	}()
	size, err := io.Copy(file, io.LimitReader(contextReader{ctx, reader}, limit+1))
	var bodyLimit *http.MaxBytesError
	if size > limit || errors.As(err, &bodyLimit) {
		return staged{}, tooLarge()
	}
	if err != nil {
		return staged{}, err
	}
	if size == 0 {
		return staged{}, rpc.Invalid("Upload body is empty")
	}
	var header [512]byte
	prefix := header[:min(size, int64(len(header)))]
	if _, err = file.ReadAt(prefix, 0); err != nil {
		return staged{}, err
	}
	if err = file.Sync(); err != nil {
		return staged{}, err
	}
	if err = file.Close(); err != nil {
		return staged{}, err
	}
	keep = true
	return staged{name, size, detectMIME(prefix, size)}, nil
}

type contextReader struct {
	context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func syncDirectory(root *os.Root, name string) error {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func syncPublished(root *os.Root, key string) error {
	leaf := path.Dir(key)
	for _, dir := range []string{leaf, path.Dir(leaf), objectsDir, stagingDir, "."} {
		if err := syncDirectory(root, dir); err != nil {
			return err
		}
	}
	return nil
}

// Purge the exact key, using a directory sync before recording the removal.
func removeStored(root *os.Root, key string) error {
	if err := objectDirectories(root, key, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := root.Remove(key); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(root, path.Dir(key))
}

func mimeType(value string) string {
	parsed, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return parsed
}

func detectMIME(header []byte, size int64) string {
	// The standard sniffer handles image and common container signatures.
	// The legacy client also accepts these raw audio formats.
	switch {
	case len(header) > len("#!AMR-WB\n") && bytes.HasPrefix(header, []byte("#!AMR-WB\n")):
		return "audio/amr-wb"
	case len(header) > len("#!AMR\n") && bytes.HasPrefix(header, []byte("#!AMR\n")):
		return "audio/amr"
	case len(header) >= 42 && bytes.HasPrefix(header, []byte("fLaC")) && header[4]&0x7f == 0 &&
		bytes.Equal(header[5:8], []byte{0, 0, 34}):
		return "audio/flac"
	case aacFrameHeader(header, size):
		return "audio/aac"
	case mpegFrameHeader(header):
		return "audio/mpeg"
	default:
		return mimeType(http.DetectContentType(header))
	}
}

func aacFrameHeader(header []byte, size int64) bool {
	if len(header) < 7 || header[0] != 0xff || header[1]&0xf6 != 0xf0 || (header[2]>>2)&0xf > 12 {
		return false
	}
	length := int64(header[3]&3)<<11 | int64(header[4])<<3 | int64(header[5]>>5)
	headerSize := int64(7)
	if header[1]&1 == 0 {
		headerSize += 2 // CRC follows the fixed header.
	}
	return length > headerSize && length <= size
}

func mpegFrameHeader(header []byte) bool {
	return len(header) > 4 && header[0] == 0xff && header[1]&0xe0 == 0xe0 &&
		header[1]&0x18 != 0x08 && header[1]&0x06 != 0 && // MPEG version and layer
		header[2]>>4 != 0 && header[2]>>4 != 0xf && header[2]&0x0c != 0x0c // Bit rate and sample rate
}

func raster(kind string) bool {
	switch kind {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "image/x-icon", "image/vnd.microsoft.icon":
		return true
	}
	return false
}

func allowedMIME(purpose, kind string) bool {
	if raster(kind) {
		return true
	}
	if purpose == "avatar" || purpose == "space" {
		return false
	}
	switch kind {
	case "application/octet-stream", "application/pdf", "application/zip", "application/x-gzip", "application/gzip",
		"text/plain", "application/ogg", "audio/ogg", "audio/mpeg", "audio/wave", "audio/wav", "audio/x-wav",
		"audio/mp4", "audio/webm", "audio/aiff", "audio/flac", "audio/aac", "audio/amr", "audio/amr-wb",
		"video/mp4", "video/webm":
		return true
	}
	return false
}

func matchesMIME(declared, detected string) bool {
	if declared == "application/octet-stream" || declared == detected {
		return true
	}
	switch declared {
	case "audio/wav", "audio/x-wav":
		return detected == "audio/wave"
	case "audio/ogg":
		return detected == "application/ogg"
	case "audio/mp4":
		return detected == "video/mp4"
	case "audio/webm":
		return detected == "video/webm"
	case "application/gzip":
		return detected == "application/x-gzip"
	case "image/vnd.microsoft.icon":
		return detected == "image/x-icon"
	}
	return false
}

// Import copies legacy bytes into the media volume and inserts a ready object.
// Use a dedicated, unpublished import volume. The caller owns reconciliation
// when its transaction rolls back or the database outcome is uncertain. An
// error can be returned with the assigned ID if publication was attempted.
func Import(ctx context.Context, db rpc.DB, root, name, purpose string, ownerID int64, src io.Reader, expiresAt *time.Time) (string, error) {
	if !validPurpose(purpose) || !validFilename(name) || ownerID <= 0 || src == nil {
		return "", rpc.Invalid("Invalid media import metadata")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	store, err := openStore(root)
	if err != nil {
		return "", err
	}
	defer store.Close()
	blob, err := stage(ctx, store, src, maxImportBytes)
	if err != nil {
		return "", err
	}
	defer store.Remove(blob.name)
	kind := blob.detected
	// Generic sniffers see only the container for these legacy voice formats.
	switch strings.ToLower(filepath.Ext(name)) {
	case ".m4a":
		if kind == "video/mp4" {
			kind = "audio/mp4"
		}
	case ".ogg", ".opus":
		if kind == "application/ogg" {
			kind = "audio/ogg"
		}
	}
	if !allowedMIME(purpose, kind) {
		if purpose == "avatar" || purpose == "space" {
			return "", badMIME()
		}
		kind = "application/octet-stream"
	}
	id := newID()
	key, err := StorageKey(id)
	if err != nil {
		return "", err
	}
	if err = objectDirectories(store, key, true); err != nil {
		return "", err
	}
	if err = store.Link(blob.name, key); err != nil {
		return "", err
	}
	if err = syncPublished(store, key); err != nil {
		_ = removeStored(store, key)
		return "", err
	}
	_, err = db.Exec(ctx, `INSERT INTO media_objects(id,owner_id,state,purpose,mime,filename,size,ready_at,expires_at)
		VALUES ($1::uuid,$2,'ready',$3,$4,$5,$6,clock_timestamp(),$7)`, id, ownerID, purpose, kind, name, blob.size, expiresAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "23") {
			return id, errors.Join(err, removeStored(store, key))
		}
		// Preserve the blob if the caller needs to reconcile an ambiguous
		// database result. Deleting it could break a committed ready row.
		return id, err
	}
	return id, nil
}
