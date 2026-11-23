package config

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SyncAdmins atomically applies an explicitly configured list. A nil list
// keeps the database's current grants; an explicitly empty list revokes them.
func SyncAdmins(ctx context.Context, pool *pgxpool.Pool, ids []int64) error {
	if ids == nil {
		return nil
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(201006352027)`); err != nil {
			return err
		}
		var missing []int64
		if err := tx.QueryRow(ctx, `SELECT ARRAY(SELECT uid FROM unnest($1::bigint[]) uid WHERE NOT EXISTS(SELECT 1 FROM accounts WHERE id=uid AND status='active'))`, ids).Scan(&missing); err != nil {
			return err
		}
		if len(missing) != 0 {
			return fmt.Errorf("CSAC_ADMIN_UIDS contains unavailable accounts: %v", missing)
		}
		_, err := tx.Exec(ctx, `UPDATE accounts SET site_admin=(id=ANY($1::bigint[])) WHERE site_admin IS DISTINCT FROM (id=ANY($1::bigint[]))`, ids)
		return err
	})
}
