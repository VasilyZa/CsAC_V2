package media

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const cleanupBatch = 128

// Cleanup performs a bounded database sweep and clears abandoned upload parts.
// It may run concurrently with HTTP and other sweepers on the same volume.
func (s *Service) Cleanup(ctx context.Context) error {
	return errors.Join(s.expireUnused(ctx), s.purgeExpired(ctx), s.purgeStaging(ctx))
}

const unused = `NOT CASE m.purpose
	WHEN 'avatar' THEN EXISTS (SELECT 1 FROM accounts a WHERE a.avatar_id=m.id)
		OR EXISTS (SELECT 1 FROM conversations c WHERE c.avatar_id=m.id)
	WHEN 'space' THEN EXISTS (SELECT 1 FROM space_post_media p WHERE p.media_id=m.id)
	WHEN 'group' THEN EXISTS (SELECT 1 FROM group_files gf WHERE gf.media_id=m.id)
	WHEN 'message' THEN EXISTS (SELECT 1 FROM messages msg WHERE msg.attachment_id=m.id AND msg.recalled_at IS NULL)
		OR EXISTS (SELECT 1 FROM stickers st WHERE st.media_id=m.id)
	ELSE true END`

func (s *Service) expireUnused(ctx context.Context) error {
	return pgx.BeginTxFunc(ctx, s.pool, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT m.id FROM media_objects m WHERE m.state='ready' AND m.expires_at IS NULL
			AND m.ready_at<=statement_timestamp()-interval '7 days' AND `+unused+`
			ORDER BY m.ready_at,m.id LIMIT $1 FOR UPDATE SKIP LOCKED`, cleanupBatch)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[pgtype.UUID])
		if err != nil || len(ids) == 0 {
			return err
		}
		// Recheck in a fresh snapshot after locking. An attachment transaction
		// may have acquired and released its share lock during selection.
		_, err = tx.Exec(ctx, `UPDATE media_objects m SET expires_at=statement_timestamp()
			WHERE m.id=ANY($1::uuid[]) AND `+unused, ids)
		return err
	})
}

func (s *Service) purgeExpired(ctx context.Context) error {
	var diskErrors []error
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text,state,stored_key FROM media_objects WHERE purged_at IS NULL AND expires_at<=statement_timestamp()
			ORDER BY expires_at,id LIMIT $1 FOR UPDATE SKIP LOCKED`, cleanupBatch)
		if err != nil {
			return err
		}
		type expired struct {
			ID    string
			State string
			Key   string
		}
		objects, err := pgx.CollectRows(rows, pgx.RowToStructByPos[expired])
		if err != nil {
			return err
		}
		for _, object := range objects {
			key, err := StorageKey(object.ID)
			if err != nil || key != object.Key {
				diskErrors = append(diskErrors, errors.New("invalid media storage key"))
				continue
			}
			if err := removeStored(s.root, key); err != nil {
				diskErrors = append(diskErrors, err)
				continue
			}
			if object.State == "pending" {
				if _, err := tx.Exec(ctx, `DELETE FROM media_objects WHERE id=$1::uuid`, object.ID); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.Exec(ctx, `DELETE FROM group_files WHERE media_id=$1::uuid`, object.ID); err != nil {
				return err
			}
			// Keep the row for historical message references and foreign keys.
			if _, err := tx.Exec(ctx, `UPDATE media_objects SET purged_at=clock_timestamp() WHERE id=$1::uuid`, object.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return errors.Join(append(diskErrors, err)...)
}

func (s *Service) purgeStaging(ctx context.Context) error {
	dir, err := s.root.Open(stagingDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	cutoff := time.Now().Add(-staleStagingAge)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := dir.ReadDir(cleanupBatch)
		for _, entry := range entries {
			id, ok := strings.CutSuffix(entry.Name(), ".part")
			if !ok || !ValidID(id) {
				continue
			}
			info, infoErr := entry.Info()
			if errors.Is(infoErr, os.ErrNotExist) {
				continue
			}
			if infoErr != nil {
				return infoErr
			}
			if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
				continue
			}
			if removeErr := s.root.Remove(filepath.Join(stagingDir, entry.Name())); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}
