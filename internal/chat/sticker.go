package chat

import (
	"context"
	"encoding/json"
	"strings"

	"csac/v2/internal/rpc"
	"csac/v2/internal/stickerart"
)

type catalogSticker struct {
	Code         string  `json:"code"`
	Label        string  `json:"label"`
	MediaID      *string `json:"media_id,omitempty"`
	FallbackText string  `json:"fallback_text"`
	DownloadPath string  `json:"download_path,omitempty"`
}

func listStickerCatalog(ctx context.Context, call rpc.Call, raw json.RawMessage) (any, error) {
	in, err := rpc.Decode[struct {
		After string `json:"after"`
	}](raw)
	if err != nil {
		return nil, err
	}
	if strings.ContainsRune(in.After, 0) {
		return nil, rpc.Invalid("Invalid sticker cursor")
	}
	rows, err := call.DB.Query(ctx, `WITH catalog AS (
		SELECT s.code,s.label,s.media_id,b.label AS builtin_label
		FROM stickers s LEFT JOIN builtin_stickers b USING (code) WHERE s.code>$1
		UNION ALL
		SELECT b.code,b.label,NULL::uuid,b.label FROM builtin_stickers b
		WHERE b.code>$1 AND NOT EXISTS (SELECT 1 FROM stickers s WHERE s.code=b.code)
	)
	SELECT c.code,c.label,m.id::text,coalesce(c.builtin_label,'')
	FROM catalog c LEFT JOIN media_objects m ON m.id=c.media_id
		AND m.purpose='message' AND m.state='ready' AND m.purged_at IS NULL
		AND (m.expires_at IS NULL OR m.expires_at>statement_timestamp())
		AND m.mime IN ('image/png','image/jpeg','image/gif','image/webp','image/bmp','image/x-icon','image/vnd.microsoft.icon')
	ORDER BY c.code LIMIT 200`, in.After)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]catalogSticker, 0)
	for rows.Next() {
		var s catalogSticker
		var builtinLabel string
		if err := rows.Scan(&s.Code, &s.Label, &s.MediaID, &builtinLabel); err != nil {
			return nil, err
		}
		s.FallbackText = s.Label
		if strings.TrimSpace(s.FallbackText) == "" {
			s.FallbackText = builtinLabel
		}
		if s.FallbackText == "" {
			s.FallbackText = s.Code
		}
		if s.MediaID != nil {
			s.DownloadPath = "/v2/media/" + *s.MediaID
		} else {
			s.DownloadPath = stickerart.DownloadPath(s.Code)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func requireSticker(ctx context.Context, db rpc.DB, code string) error {
	var found bool
	if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM stickers WHERE code=$1
		UNION ALL SELECT 1 FROM builtin_stickers WHERE code=$1)`, code).Scan(&found); err != nil {
		return err
	}
	if !found {
		return rpc.NotFound("Sticker unavailable")
	}
	return nil
}
