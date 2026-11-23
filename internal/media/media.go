// Package media stores immutable blobs and authorizes downloads through live references.
package media

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"csac/v2/internal/rpc"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	mediaPath            = "/v2/media/"
	pendingTTL           = 15 * time.Minute
	defaultFileRetention = 72 * time.Hour
)

// Service owns a handle to the private storage directory. Close it after the
// HTTP server and any scheduled Cleanup calls have stopped.
type Service struct {
	pool          *pgxpool.Pool
	root          *os.Root
	maxBytes      int64
	fileRetention time.Duration
	extensions    map[string]struct{}
}

type Options struct {
	MaxBytes       int64
	FileRetention  time.Duration
	FileExtensions []string
}

func New(pool *pgxpool.Pool, root string, maxBytes int64) (*Service, error) {
	return Open(pool, root, Options{MaxBytes: maxBytes})
}

func Open(pool *pgxpool.Pool, root string, options Options) (*Service, error) {
	if pool == nil || options.MaxBytes < 1 || options.MaxBytes == math.MaxInt64 {
		return nil, errors.New("media requires a database pool and a positive bounded upload size")
	}
	if options.FileRetention == 0 {
		options.FileRetention = defaultFileRetention
	}
	if options.FileRetention < time.Second || options.FileRetention > 3650*24*time.Hour {
		return nil, errors.New("media file retention must be between one second and 3650 days")
	}
	store, err := openStore(root)
	if err != nil {
		return nil, err
	}
	extensions := make(map[string]struct{}, len(options.FileExtensions))
	for _, extension := range options.FileExtensions {
		extensions[strings.TrimPrefix(strings.ToLower(strings.TrimSpace(extension)), ".")] = struct{}{}
	}
	return &Service{pool: pool, root: store, maxBytes: options.MaxBytes, fileRetention: options.FileRetention, extensions: extensions}, nil
}

func fileAttachment(purpose, mime string, asFile bool) bool {
	return purpose == "group" || (purpose == "message" && (asFile || (!raster(mime) && !strings.HasPrefix(mime, "audio/"))))
}

func (s *Service) Close() error { return s.root.Close() }

func (s *Service) Register(reg *rpc.Registry) {
	reg.Register("media.prepare", rpc.Method{Access: rpc.Member, Write: true, Dedup: true, Handle: s.prepare})
	reg.Register("media.info", rpc.Method{Access: rpc.Public, Handle: s.info})
	registerGroupFiles(reg)
}

type Object struct {
	ID           string     `json:"id"`
	Purpose      string     `json:"purpose"`
	MIME         string     `json:"mime"`
	Filename     string     `json:"filename"`
	Size         int64      `json:"size"`
	AsFile       bool       `json:"as_file"`
	ReadyAt      time.Time  `json:"ready_at"`
	ExpiresAt    *time.Time `json:"expires_at"`
	DownloadPath string     `json:"download_path"`
}

type preparation struct {
	ID         string    `json:"id"`
	UploadPath string    `json:"upload_path"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (s *Service) prepare(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		Purpose  string `json:"purpose"`
		MIME     string `json:"mime"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
		AsFile   bool   `json:"as_file"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if !validPurpose(in.Purpose) || !validFilename(in.Filename) || in.Size <= 0 {
		return nil, rpc.Invalid("A valid purpose, filename, and positive size are required")
	}
	if in.Size > s.maxBytes {
		return nil, tooLarge()
	}
	declared := mimeType(in.MIME)
	if !allowedMIME(in.Purpose, declared) {
		return nil, badMIME()
	}
	if in.AsFile && in.Purpose != "message" {
		return nil, rpc.Invalid("as_file is available for message attachments")
	}
	if len(s.extensions) != 0 && fileAttachment(in.Purpose, declared, in.AsFile) {
		extension := strings.TrimPrefix(strings.ToLower(path.Ext(in.Filename)), ".")
		if _, allowed := s.extensions[extension]; !allowed {
			return nil, badMIME()
		}
	}
	var out preparation
	err = call.DB.QueryRow(ctx, `INSERT INTO media_objects(owner_id,purpose,mime,filename,size,expires_at,as_file)
		VALUES ($1,$2,$3,$4,$5,clock_timestamp()+$6*interval '1 second',$7) RETURNING id::text,expires_at`,
		call.Actor.ID, in.Purpose, declared, in.Filename, in.Size, int(pendingTTL.Seconds()), in.AsFile).Scan(&out.ID, &out.ExpiresAt)
	out.UploadPath = mediaPath + out.ID
	return out, err
}

func (s *Service) info(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		ID string `json:"id"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if !ValidID(in.ID) {
		return nil, rpc.Invalid("Expected a media ID")
	}
	object, _, err := findReadable(ctx, call.DB, in.ID, call.Actor)
	return object, err
}

// ValidID recognizes the same canonical UUID form used in storage and URLs.
func ValidID(id string) bool {
	if len(id) != 36 {
		return false
	}
	var parsed pgtype.UUID
	return parsed.Scan(id) == nil && parsed.String() == id && parsed.Bytes != [16]byte{}
}

// RequireOwnedReady locks a reusable object for a caller creating a new
// reference. Pass the transaction which will insert the reference.
func RequireOwnedReady(ctx context.Context, db rpc.DB, ownerID int64, id, purpose string) error {
	if !ValidID(id) {
		return rpc.Invalid("Expected a media ID")
	}
	var ok bool
	err := db.QueryRow(ctx, `SELECT true FROM media_objects m
		WHERE m.id=$1::uuid AND m.owner_id=$2 AND m.purpose=$3 AND `+readyPredicate+` FOR SHARE`, id, ownerID, purpose).Scan(&ok)
	return unavailable(err)
}

const readyPredicate = `m.state='ready' AND m.purged_at IS NULL AND (m.expires_at IS NULL OR m.expires_at>statement_timestamp())`

// The metadata and HTTP handlers use one statement so authorization and the
// returned metadata come from the same database snapshot.
func findReadable(ctx context.Context, db rpc.DB, id string, actor rpc.Actor) (Object, string, error) {
	var m Object
	var key string
	err := db.QueryRow(ctx, `SELECT m.id::text,m.purpose,m.mime,m.filename,m.size,m.ready_at,m.expires_at,m.as_file,m.stored_key
		FROM media_objects m WHERE m.id=$1::uuid AND `+readyPredicate+` AND (
			m.owner_id=$2
			OR (m.purpose='avatar' AND (
				EXISTS (SELECT 1 FROM accounts a WHERE a.avatar_id=m.id AND a.status='active')
				OR EXISTS (SELECT 1 FROM conversations c WHERE c.avatar_id=m.id AND c.kind='group' AND c.archived_at IS NULL
					AND (c.visibility='public' OR ($2>0 AND $3 AND (
						EXISTS (SELECT 1 FROM conversation_members cm WHERE cm.conversation_id=c.id AND cm.user_id=$2)
						OR EXISTS (SELECT 1 FROM active_site_admins WHERE user_id=$2)))))
			))
			OR ($2>0 AND $3 AND CASE m.purpose
				WHEN 'message' THEN EXISTS (
					SELECT 1 FROM messages msg JOIN chat_reviewers r ON r.conversation_id=msg.conversation_id AND r.user_id=$2
					WHERE msg.attachment_id=m.id AND msg.recalled_at IS NULL
				) OR EXISTS (SELECT 1 FROM stickers st WHERE st.media_id=m.id)
				WHEN 'group' THEN EXISTS (
					SELECT 1 FROM group_files gf JOIN conversations c ON c.id=gf.conversation_id
					LEFT JOIN conversation_members cm ON cm.conversation_id=c.id AND cm.user_id=$2
					WHERE gf.media_id=m.id AND c.kind='group' AND c.archived_at IS NULL
					AND (cm.user_id IS NOT NULL OR EXISTS (SELECT 1 FROM active_site_admins WHERE user_id=$2))
				)
				WHEN 'space' THEN EXISTS (
					SELECT 1 FROM space_post_media link JOIN space_posts p ON p.id=link.post_id
					LEFT JOIN space_posts root ON root.id=p.parent_id
					WHERE link.media_id=m.id AND root.parent_id IS NULL
					AND space_can_view_item($2,coalesce(root.author_id,p.author_id),p.author_id)
				)
				ELSE false END)
		)`, id, actor.ID, actor.Chat).Scan(&m.ID, &m.Purpose, &m.MIME, &m.Filename, &m.Size, &m.ReadyAt, &m.ExpiresAt, &m.AsFile, &key)
	m.DownloadPath = mediaPath + m.ID
	return m, key, unavailable(err)
}

func validPurpose(purpose string) bool {
	switch purpose {
	case "avatar", "message", "group", "space":
		return true
	}
	return false
}

func validFilename(name string) bool {
	return len(name) > 0 && len(name) <= 255 && utf8.ValidString(name) && name != "." && name != ".." &&
		strings.TrimSpace(name) == name && !strings.ContainsAny(name, `/\`) && strings.IndexFunc(name, unicode.IsControl) == -1
}

func unavailable(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return rpc.NotFound("Media unavailable")
	}
	return err
}

func tooLarge() error {
	return rpc.Error("media_too_large", "Upload exceeds its size limit", http.StatusRequestEntityTooLarge)
}

func badMIME() error {
	return rpc.Error("unsupported_media_type", "Unsupported or mismatched media type", http.StatusUnsupportedMediaType)
}
