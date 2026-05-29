package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	*sql.DB
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := ensureDir(dir); err != nil {
			return nil, fmt.Errorf("ensure db dir: %w", err)
		}
	}

	// _journal=WAL for concurrent reads, _busy_timeout for retries under write contention,
	// _foreign_keys=on because nothing here is large enough to justify the footgun of it being off.
	dsn := fmt.Sprintf("file:%s?_journal=WAL&_busy_timeout=5000&_foreign_keys=on", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sql ping: %w", err)
	}

	// modernc/sqlite handles concurrency in-process, but the single-writer constraint
	// of SQLite itself means we want at most one writer-shaped connection.
	db.SetMaxOpenConns(1)

	return &Store{DB: db}, nil
}

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at INTEGER NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}

	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	for _, name := range files {
		var exists int
		if err := s.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM schema_migrations WHERE version = ?`, name).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if exists > 0 {
			continue
		}

		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := s.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("exec migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations(version, applied_at) VALUES (?, unixepoch())`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
		slog.Info("migration applied", "version", name)
	}

	return nil
}
