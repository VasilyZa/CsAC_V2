package media

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
)

// HTTP serves the full /v2/media/{id} path. The caller authenticates the
// bearer token before passing the actor, or passes a zero actor for an
// anonymous download of a currently public avatar.
func (s *Service) HTTP(w http.ResponseWriter, r *http.Request, actor rpc.Actor) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	id, ok := strings.CutPrefix(r.URL.EscapedPath(), mediaPath)
	if !ok || !ValidID(id) {
		writeHTTPError(w, rpc.NotFound("Media unavailable"))
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.put(w, r, actor, id)
	case http.MethodGet, http.MethodHead:
		s.get(w, r, actor, id)
	default:
		w.Header().Set("Allow", "GET, HEAD, PUT")
		writeHTTPError(w, rpc.Error("method_not_allowed", "Method not allowed", http.StatusMethodNotAllowed))
	}
}

type uploadSpec struct {
	purpose string
	mime    string
	size    int64
	asFile  bool
}

func ownedPending(ctx context.Context, db rpc.DB, id string, ownerID int64, lock bool) (uploadSpec, error) {
	var spec uploadSpec
	var state string
	query := `SELECT purpose,mime,size,as_file,state FROM media_objects WHERE id=$1::uuid AND owner_id=$2
		AND purged_at IS NULL AND (expires_at IS NULL OR expires_at>clock_timestamp())`
	if lock {
		query += ` FOR UPDATE`
	}
	err := db.QueryRow(ctx, query, id, ownerID).Scan(&spec.purpose, &spec.mime, &spec.size, &spec.asFile, &state)
	if err == nil && state != "pending" {
		err = rpc.Conflict("Media is already ready")
	}
	return spec, unavailable(err)
}

func (s *Service) put(w http.ResponseWriter, r *http.Request, actor rpc.Actor, id string) {
	if actor.ID <= 0 {
		writeHTTPError(w, rpc.Unauthenticated())
		return
	}
	if !actor.Chat {
		writeHTTPError(w, rpc.Error("verification_required", "Complete account verification", http.StatusForbidden))
		return
	}
	ctx := r.Context()
	spec, err := ownedPending(ctx, s.pool, id, actor.ID, false)
	if err != nil {
		writeHTTPError(w, err)
		return
	}
	if spec.size > s.maxBytes || r.ContentLength > spec.size {
		writeHTTPError(w, tooLarge())
		return
	}
	if (r.ContentLength >= 0 && r.ContentLength != spec.size) || r.Header.Get("Content-Range") != "" {
		writeHTTPError(w, rpc.Invalid("Expected the full prepared upload size"))
		return
	}
	if contentType := r.Header.Get("Content-Type"); contentType != "" && mimeType(contentType) != spec.mime {
		writeHTTPError(w, badMIME())
		return
	}
	if encoding := r.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		writeHTTPError(w, badMIME())
		return
	}
	body := http.MaxBytesReader(w, r.Body, spec.size)
	defer body.Close()
	blob, err := stage(ctx, s.root, body, spec.size)
	if err != nil {
		writeHTTPError(w, err)
		return
	}
	defer s.root.Remove(blob.name)
	if blob.size != spec.size {
		writeHTTPError(w, rpc.Invalid("Expected the full prepared upload size"))
		return
	}
	if !allowedMIME(spec.purpose, spec.mime) || !matchesMIME(spec.mime, blob.detected) {
		writeHTTPError(w, badMIME())
		return
	}

	object, err := s.finalize(ctx, id, actor.ID, spec, blob)
	if err != nil {
		writeHTTPError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", object.DownloadPath)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(object)
}

func (s *Service) finalize(ctx context.Context, id string, ownerID int64, spec uploadSpec, blob staged) (Object, error) {
	var object Object
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := ownedPending(ctx, tx, id, ownerID, true)
		if err != nil {
			return err
		}
		if current != spec {
			return rpc.Conflict("Upload metadata changed")
		}
		var ttl *int
		if fileAttachment(spec.purpose, spec.mime, spec.asFile) {
			seconds := int(s.fileRetention.Seconds())
			ttl = &seconds
		}
		var stored string
		err = tx.QueryRow(ctx, `UPDATE media_objects SET state='ready',ready_at=statement_timestamp(),
			expires_at=statement_timestamp()+$2*interval '1 second'
			WHERE id=$1::uuid AND expires_at>statement_timestamp()
			RETURNING id::text,purpose,mime,filename,size,ready_at,expires_at,as_file,stored_key`, id, ttl).
			Scan(&object.ID, &object.Purpose, &object.MIME, &object.Filename, &object.Size, &object.ReadyAt, &object.ExpiresAt, &object.AsFile, &stored)
		if err != nil {
			return unavailable(err)
		}
		key, err := StorageKey(id)
		if err != nil || key != stored {
			return errors.New("invalid media storage key")
		}
		if err := objectDirectories(s.root, key, true); err != nil {
			return err
		}
		if err := s.root.Rename(blob.name, key); err != nil {
			return err
		}
		// A failed or ambiguous commit leaves the file available to the ready
		// row or to the pending-row expiry sweep. A retry may replace it only
		// while the row still holds the pending state.
		return syncPublished(s.root, key)
	})
	object.DownloadPath = mediaPath + object.ID
	return object, err
}

func (s *Service) get(w http.ResponseWriter, r *http.Request, actor rpc.Actor, id string) {
	object, key, err := findReadable(r.Context(), s.pool, id, actor)
	if err != nil {
		writeHTTPError(w, err)
		return
	}
	expected, err := StorageKey(id)
	if err != nil || key != expected {
		writeHTTPError(w, errors.New("invalid media storage key"))
		return
	}
	if err := objectDirectories(s.root, key, false); err != nil {
		writeHTTPError(w, rpc.NotFound("Media unavailable"))
		return
	}
	entry, err := s.root.Lstat(key)
	if err != nil || !entry.Mode().IsRegular() {
		writeHTTPError(w, rpc.NotFound("Media unavailable"))
		return
	}
	file, err := s.root.Open(key)
	if err != nil {
		writeHTTPError(w, rpc.NotFound("Media unavailable"))
		return
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !os.SameFile(entry, info) || !info.Mode().IsRegular() || info.Size() != object.Size {
		writeHTTPError(w, rpc.NotFound("Media unavailable"))
		return
	}
	disposition, contentType := "attachment", object.MIME
	if !allowedMIME(object.Purpose, contentType) {
		contentType = "application/octet-stream"
	} else if object.Purpose != "group" && !object.AsFile && (raster(contentType) || strings.HasPrefix(contentType, "audio/")) {
		disposition = "inline"
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": object.Filename}))
	w.Header().Set("Content-Type", contentType)
	http.ServeContent(w, r, object.Filename, time.Time{}, file)
}

func writeHTTPError(w http.ResponseWriter, err error) {
	var fault *rpc.Fault
	if !errors.As(err, &fault) {
		fault = &rpc.Fault{Code: "internal", Message: "Media operation failed", Status: http.StatusInternalServerError}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(fault.Status)
	_ = json.NewEncoder(w).Encode(struct {
		Error *rpc.Fault `json:"error"`
	}{Error: fault})
}
