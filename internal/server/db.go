package server

import (
	"database/sql"
	"fmt"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS orgs (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL UNIQUE,
	enroll_token TEXT NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS devices (
	id            TEXT PRIMARY KEY,
	org_id        TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	secret_hash   TEXT NOT NULL,
	machine_id    TEXT NOT NULL DEFAULT '',
	hostname      TEXT NOT NULL,
	display_name  TEXT NOT NULL DEFAULT '',
	os            TEXT NOT NULL DEFAULT '',
	platform      TEXT NOT NULL DEFAULT '',
	agent_version TEXT NOT NULL DEFAULT '',
	remote_ip     TEXT NOT NULL DEFAULT '',
	sysinfo       TEXT NOT NULL DEFAULT '{}',
	metrics       TEXT NOT NULL DEFAULT '{}',
	notes         TEXT NOT NULL DEFAULT '',
	enrolled_at   INTEGER NOT NULL,
	last_seen     INTEGER NOT NULL DEFAULT 0,
	refresh_inv   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_devices_org ON devices(org_id);

CREATE TABLE IF NOT EXISTS metrics_history (
	device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	ts        INTEGER NOT NULL,
	cpu       REAL NOT NULL,
	mem       REAL NOT NULL,
	disk      REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_mh_dev_ts ON metrics_history(device_id, ts);

CREATE TABLE IF NOT EXISTS scripts (
	id          TEXT PRIMARY KEY,
	name        TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	shell       TEXT NOT NULL,
	body        TEXT NOT NULL,
	os          TEXT NOT NULL DEFAULT 'any',
	timeout_sec INTEGER NOT NULL DEFAULT 300,
	builtin     INTEGER NOT NULL DEFAULT 0,
	created_at  INTEGER NOT NULL,
	updated_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS jobs (
	id          TEXT PRIMARY KEY,
	device_id   TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	type        TEXT NOT NULL,
	name        TEXT NOT NULL DEFAULT '',
	shell       TEXT NOT NULL DEFAULT '',
	script      TEXT NOT NULL DEFAULT '',
	timeout_sec INTEGER NOT NULL DEFAULT 300,
	status      TEXT NOT NULL DEFAULT 'queued', -- queued, sent, done, failed, cancelled
	exit_code   INTEGER,
	output      TEXT NOT NULL DEFAULT '',
	error       TEXT NOT NULL DEFAULT '',
	created_by  TEXT NOT NULL DEFAULT '',
	created_at  INTEGER NOT NULL,
	sent_at     INTEGER,
	finished_at INTEGER
);
CREATE INDEX IF NOT EXISTS idx_jobs_dev ON jobs(device_id, status);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);

CREATE TABLE IF NOT EXISTS sessions (
	token_hash TEXT PRIMARY KEY,
	username   TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS audit (
	ts      INTEGER NOT NULL,
	actor   TEXT NOT NULL,
	action  TEXT NOT NULL,
	target  TEXT NOT NULL DEFAULT '',
	detail  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit(ts);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

func openDB(dataDir string) (*sql.DB, error) {
	path := filepath.Join(dataDir, "oceanic.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite: serialize writers; plenty for a homelab.
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return db, nil
}
