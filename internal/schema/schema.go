// Package schema applies the versioned PostgreSQL schema in one transaction.
package schema

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed sql/*.sql
var scripts embed.FS

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	entries, err := scripts.ReadDir("sql")
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(201006352026)`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name text PRIMARY KEY, sha256 text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			code, err := scripts.ReadFile("sql/" + name)
			if err != nil {
				return err
			}
			hash := sha256.Sum256(code)
			want := hex.EncodeToString(hash[:])
			var recorded string
			err = tx.QueryRow(ctx, `SELECT sha256 FROM schema_migrations WHERE name=$1`, name).Scan(&recorded)
			if err == nil {
				if recorded != want {
					return fmt.Errorf("applied migration %s changed", name)
				}
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if _, err := tx.Exec(ctx, string(code), pgx.QueryExecModeSimpleProtocol); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name,sha256) VALUES ($1,$2)`, name, want); err != nil {
				return err
			}
		}
		return nil
	})
}
