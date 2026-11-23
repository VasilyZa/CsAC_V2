package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	purgeAccountLimit   = 16
	purgeSweepTimeout   = 45 * time.Second
	purgeAccountTimeout = 10 * time.Second
)

// The selection and completion are in the same transaction. If an account
// fails, delay its next attempt in a separate transaction so it cannot block
// other eligible accounts. The delay is independent of the recovery deadline.
func (s *Service) purgeExpiredAccounts(ctx context.Context) error {
	work, cancel := context.WithTimeout(ctx, purgeSweepTimeout)
	defer cancel()
	var failures []error
	for range purgeAccountLimit {
		if err := work.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		id, err := s.purgeNextAccount(work)
		if err == nil {
			if id == 0 {
				break
			}
			continue
		}
		failures = append(failures, fmt.Errorf("purge account %d: %w", id, err))
		if id == 0 {
			break
		}
		if err := s.deferAccountPurge(work, id); err != nil {
			failures = append(failures, fmt.Errorf("defer purge for account %d: %w", id, err))
			break
		}
	}
	return errors.Join(failures...)
}

func (s *Service) deferAccountPurge(ctx context.Context, id int64) error {
	retry, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := s.pool.Exec(retry, `WITH pending AS (
		SELECT id FROM accounts WHERE id=$1 AND status='deleted' AND content_purged_at IS NULL
		FOR NO KEY UPDATE SKIP LOCKED
	) UPDATE accounts a SET content_purge_retry_at=statement_timestamp()+interval '5 minutes'
	FROM pending WHERE a.id=pending.id`, id)
	return err
}

func (s *Service) purgeNextAccount(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, purgeAccountTimeout)
	defer cancel()
	var id int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='750ms'`, pgx.QueryExecModeSimpleProtocol); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `SELECT id FROM accounts WHERE status='deleted' AND content_purged_at IS NULL
			AND (purged_at IS NOT NULL OR deleted_at<=statement_timestamp()-$1::bigint*interval '1 second')
			AND (content_purge_retry_at IS NULL OR content_purge_retry_at<=statement_timestamp())
			ORDER BY coalesce(deleted_at,purged_at),id LIMIT 1 FOR UPDATE SKIP LOCKED`, int64(coolingPeriod.Seconds())).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return purgeAccountContent(ctx, tx, id)
	})
	return id, err
}

// Order conversation locks by ID, before touching any dependent rows. Chat,
// group writes and group-file writes already lock the conversation first.
// Re-evaluate ownership once locked in case a transfer committed while waiting.
func purgeAccountContent(ctx context.Context, tx pgx.Tx, id int64) error {
	if _, err := tx.Exec(ctx, lockPurgeConversations, id); err != nil {
		return fmt.Errorf("lock conversations: %w", err)
	}
	if _, err := tx.Exec(ctx, lockPurgeFriendPairs, id); err != nil {
		return fmt.Errorf("lock friend pairs: %w", err)
	}
	// Group files and other rows can reference these objects. Take the media
	// locks before deleting those rows, matching the storage sweeper. Check
	// for surviving references after the dependent rows have been removed.
	if _, err := tx.Exec(ctx, `SELECT m.id FROM media_objects m WHERE m.owner_id=$1 AND m.purged_at IS NULL
		AND (m.expires_at IS NULL OR m.expires_at>statement_timestamp()) ORDER BY m.id FOR UPDATE`, id); err != nil {
		return fmt.Errorf("lock account media: %w", err)
	}
	for _, step := range accountContentPurgeSteps {
		if _, err := tx.Exec(ctx, step.sql, id); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE media_objects m SET expires_at=statement_timestamp()
		WHERE m.owner_id=$1 AND m.purged_at IS NULL AND (m.expires_at IS NULL OR m.expires_at>statement_timestamp())
		AND `+orphanedAccountMedia, id); err != nil {
		return fmt.Errorf("expire orphan media: %w", err)
	}
	_, err := tx.Exec(ctx, `UPDATE accounts a SET
		username=CASE WHEN EXISTS(SELECT 1 FROM accounts other WHERE lower(other.username COLLATE "C")=':'||a.id AND other.id<>a.id)
			THEN ':'||a.id||':'||gen_random_uuid() ELSE ':'||a.id END,
		nickname='Deleted account',password_hash='',email=NULL,email_verified=false,registration_grant=false,
		site_admin=false,avatar_id=NULL,bio='',ban_until=NULL,ban_reason='',allow_auto_join=false,
		pat_action=$2,is_first_login=false,purged_at=coalesce(purged_at,statement_timestamp()),
		content_purged_at=statement_timestamp(),content_purge_retry_at=NULL WHERE a.id=$1`, id, defaultPatAction)
	return err
}
