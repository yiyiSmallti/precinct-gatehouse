// Package store opens the local SQLite database (pure Go, no cgo) that holds
// issued tokens and usage records.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS tokens (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  issued_at   INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL,
  scope       TEXT NOT NULL,
  revoked_at  INTEGER
);
CREATE TABLE IF NOT EXISTS usage (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id        TEXT NOT NULL,
  ts                INTEGER NOT NULL,
  day               TEXT NOT NULL,
  token_id          TEXT NOT NULL,
  token_name        TEXT NOT NULL,
  requested_model   TEXT NOT NULL,
  model             TEXT NOT NULL,
  target            TEXT NOT NULL,
  prompt_tokens     INTEGER NOT NULL,
  completion_tokens INTEGER NOT NULL,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  cost              REAL NOT NULL,
  status            INTEGER NOT NULL,
  latency_ms        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS usage_token_day ON usage(token_id, day);
CREATE TABLE IF NOT EXISTS api_keys (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  key_hash     TEXT NOT NULL UNIQUE,
  key_prefix   TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL DEFAULT 0,
  enabled      INTEGER NOT NULL DEFAULT 1,
  models       TEXT NOT NULL DEFAULT '[]',
  groups       TEXT NOT NULL DEFAULT '[]',
  rpm          INTEGER NOT NULL DEFAULT 0,
  rpd          INTEGER NOT NULL DEFAULT 0,
  concurrency  INTEGER NOT NULL DEFAULT 0,
  note         TEXT NOT NULL DEFAULT '',
  last_used_at INTEGER NOT NULL DEFAULT 0,
  deleted_at   INTEGER
);
CREATE TABLE IF NOT EXISTS request_log (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  request_id        TEXT NOT NULL,
  ts                INTEGER NOT NULL,
  day               TEXT NOT NULL,
  key_id            TEXT NOT NULL DEFAULT '',
  key_name          TEXT NOT NULL DEFAULT '',
  protocol          TEXT NOT NULL DEFAULT '',
  client_ip         TEXT NOT NULL DEFAULT '',
  requested_model   TEXT NOT NULL DEFAULT '',
  model             TEXT NOT NULL DEFAULT '',
  status            INTEGER NOT NULL,
  error_code        TEXT NOT NULL DEFAULT '',
  stream            INTEGER NOT NULL DEFAULT 0,
  prompt_tokens     INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens     INTEGER NOT NULL DEFAULT 0,
  latency_ms        INTEGER NOT NULL DEFAULT 0,
  ttft_ms           INTEGER NOT NULL DEFAULT 0,
  admitted          INTEGER NOT NULL DEFAULT 0,
  redaction_mode    TEXT NOT NULL DEFAULT '',
  redaction_count   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS request_log_key_day ON request_log(key_id, day);
CREATE INDEX IF NOT EXISTS request_log_ts ON request_log(ts);
CREATE TABLE IF NOT EXISTS settings (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
`

// Open opens (and migrates) <dataDir>/gatehouse.db.
func Open(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dataDir, "gatehouse.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	if err := ensureColumn(db, "usage", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	if err := ensureColumn(db, "request_log", "redaction_mode", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	if err := ensureColumn(db, "request_log", "redaction_count", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return db, nil
}

func ensureColumn(db *sql.DB, table, col, def string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec("ALTER TABLE " + table + " ADD COLUMN " + col + " " + def)
	return err
}
