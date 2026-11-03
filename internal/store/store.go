// Package store persists Tideline data in SQLite.
//
// Documents (entries, treatments, device status, profiles, food and client
// settings) are stored as the JSON the uploader sent, alongside a few indexed
// columns used for querying. Keeping the original document means Nightscout
// clients get back exactly the fields they wrote, including ones Tideline
// doesn't know about.
//
// Every document also carries server-managed metadata used by Nightscout API
// v3 sync: srv_created, srv_modified (strictly increasing across the whole
// store) and is_valid (deletions are soft, so syncing clients learn about them
// through the history endpoint).
package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // SQLite driver (pure Go)
)

// Doc is a JSON document as exchanged with Nightscout clients.
type Doc = map[string]any

// ChangeFunc is called after documents are written to a collection.
// Deleted documents are reported as {"_id": id, "identifier": id, "_deleted": true}.
type ChangeFunc func(collection string, docs []Doc)

// Store is a SQLite-backed document store.
type Store struct {
	db *sql.DB

	mu        sync.RWMutex
	listeners []ChangeFunc

	modMu   sync.Mutex
	lastMod int64
}

// baseSchema is the v1.0 schema. Later changes are applied as migrations so
// existing databases upgrade in place.
const baseSchema = `
CREATE TABLE IF NOT EXISTS entries (
	id         TEXT PRIMARY KEY,
	type       TEXT NOT NULL,
	date       INTEGER NOT NULL,
	sgv        REAL,
	direction  TEXT,
	device     TEXT,
	doc        TEXT NOT NULL,
	srv_created INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS entries_type_date ON entries(type, date);
CREATE INDEX IF NOT EXISTS entries_date ON entries(date);

CREATE TABLE IF NOT EXISTS treatments (
	id          TEXT PRIMARY KEY,
	event_type  TEXT NOT NULL,
	created_at  INTEGER NOT NULL,
	doc         TEXT NOT NULL,
	srv_created INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS treatments_type_created ON treatments(event_type, created_at);
CREATE INDEX IF NOT EXISTS treatments_created ON treatments(created_at);

CREATE TABLE IF NOT EXISTS devicestatus (
	id          TEXT PRIMARY KEY,
	created_at  INTEGER NOT NULL,
	device      TEXT NOT NULL,
	doc         TEXT NOT NULL,
	srv_created INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS devicestatus_device_created ON devicestatus(device, created_at);
CREATE INDEX IF NOT EXISTS devicestatus_created ON devicestatus(created_at);

CREATE TABLE IF NOT EXISTS profile (
	id          TEXT PRIMARY KEY,
	created_at  INTEGER NOT NULL,
	doc         TEXT NOT NULL,
	srv_created INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS profile_created ON profile(created_at);

CREATE TABLE IF NOT EXISTS tokens (
	id           TEXT PRIMARY KEY,
	name         TEXT NOT NULL,
	role         TEXT NOT NULL,
	hash         TEXT NOT NULL UNIQUE,
	sha1         TEXT NOT NULL UNIQUE,
	prefix       TEXT NOT NULL,
	created_at   INTEGER NOT NULL,
	expires_at   INTEGER,
	last_used_at INTEGER,
	revoked_at   INTEGER
);

CREATE TABLE IF NOT EXISTS settings (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS audit (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	at     INTEGER NOT NULL,
	actor  TEXT NOT NULL,
	action TEXT NOT NULL,
	detail TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_at ON audit(at);
`

// migrations[i] upgrades a database from user_version i to i+1.
var migrations = []string{
	// 1: API v3 sync metadata, soft deletes, food and client settings collections.
	`
	ALTER TABLE entries      ADD COLUMN srv_modified INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE entries      ADD COLUMN is_valid     INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE treatments   ADD COLUMN srv_modified INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE treatments   ADD COLUMN is_valid     INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE devicestatus ADD COLUMN srv_modified INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE devicestatus ADD COLUMN is_valid     INTEGER NOT NULL DEFAULT 1;
	ALTER TABLE profile      ADD COLUMN srv_modified INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE profile      ADD COLUMN is_valid     INTEGER NOT NULL DEFAULT 1;
	UPDATE entries      SET srv_modified = srv_created;
	UPDATE treatments   SET srv_modified = srv_created;
	UPDATE devicestatus SET srv_modified = srv_created;
	UPDATE profile      SET srv_modified = srv_created;
	CREATE INDEX entries_modified      ON entries(srv_modified);
	CREATE INDEX treatments_modified   ON treatments(srv_modified);
	CREATE INDEX devicestatus_modified ON devicestatus(srv_modified);
	CREATE INDEX profile_modified      ON profile(srv_modified);

	CREATE TABLE food (
		id           TEXT PRIMARY KEY,
		created_at   INTEGER NOT NULL,
		doc          TEXT NOT NULL,
		srv_created  INTEGER NOT NULL,
		srv_modified INTEGER NOT NULL,
		is_valid     INTEGER NOT NULL DEFAULT 1
	);
	CREATE INDEX food_modified ON food(srv_modified);

	CREATE TABLE ns_settings (
		id           TEXT PRIMARY KEY,
		created_at   INTEGER NOT NULL,
		doc          TEXT NOT NULL,
		srv_created  INTEGER NOT NULL,
		srv_modified INTEGER NOT NULL,
		is_valid     INTEGER NOT NULL DEFAULT 1
	);
	CREATE INDEX ns_settings_modified ON ns_settings(srv_modified);
	`,
	// 2: Web Push device registrations.
	`
	CREATE TABLE push_subscriptions (
		id              TEXT PRIMARY KEY,
		endpoint        TEXT NOT NULL UNIQUE,
		p256dh          TEXT NOT NULL,
		auth            TEXT NOT NULL,
		token_id        TEXT,
		owner           TEXT NOT NULL,
		name            TEXT NOT NULL,
		min_level       TEXT NOT NULL,
		escalate_after  INTEGER NOT NULL DEFAULT 0,
		created_at      INTEGER NOT NULL,
		last_success_at INTEGER,
		failures        INTEGER NOT NULL DEFAULT 0
	);
	`,
}

// Open opens (creating if needed) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path
	if path == ":memory:" {
		dsn = "file::memory:"
	}
	dsn += "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes and keeps in-memory DBs coherent.
	// Tideline serves one person's data, so this is plenty.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.initLastMod(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	if _, err := s.db.Exec(baseSchema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) initLastMod() error {
	var parts []string
	for _, c := range collections {
		parts = append(parts, `SELECT COALESCE(MAX(srv_modified), 0) AS m FROM `+c.table)
	}
	return s.db.QueryRow(`SELECT MAX(m) FROM (` + strings.Join(parts, " UNION ALL ") + `)`).Scan(&s.lastMod)
}

// nextModified returns a strictly increasing millisecond timestamp, so
// clients paging through history with "modified after X" never miss a
// document that shares a millisecond with another.
func (s *Store) nextModified() int64 {
	s.modMu.Lock()
	defer s.modMu.Unlock()
	now := time.Now().UnixMilli()
	if now <= s.lastMod {
		now = s.lastMod + 1
	}
	s.lastMod = now
	return now
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// OnChange registers a listener for writes.
func (s *Store) OnChange(f ChangeFunc) {
	s.mu.Lock()
	s.listeners = append(s.listeners, f)
	s.mu.Unlock()
}

func (s *Store) emit(coll string, docs []Doc) {
	if len(docs) == 0 {
		return
	}
	s.mu.RLock()
	ls := append([]ChangeFunc{}, s.listeners...)
	s.mu.RUnlock()
	for _, l := range ls {
		l(coll, docs)
	}
}

// ---------------------------------------------------------------------------
// IDs and value helpers

// NewID returns a 24-hex-character ID shaped like a MongoDB ObjectId, which
// some Nightscout clients expect.
func NewID() string {
	var b [12]byte
	binary.BigEndian.PutUint32(b[:4], uint32(time.Now().Unix()))
	if _, err := rand.Read(b[4:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// toFloat converts a JSON value (number, json.Number, numeric string) to float64.
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.000Z0700",
	"2006-01-02T15:04:05Z0700",
	"2006-01-02T15:04:05.000",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	time.RFC1123Z,
	time.RFC1123,
	"Mon Jan 02 2006 15:04:05 GMT-0700",
}

// ParseTime parses the assorted timestamp formats Nightscout uploaders send:
// epoch milliseconds (number or string), epoch seconds, or ISO 8601 strings.
func ParseTime(v any) (time.Time, bool) {
	if v == nil {
		return time.Time{}, false
	}
	if f, ok := toFloat(v); ok {
		if f <= 0 {
			return time.Time{}, false
		}
		if f < 1e11 { // seconds
			return time.Unix(int64(f), 0).UTC(), true
		}
		return time.UnixMilli(int64(f)).UTC(), true
	}
	s, ok := v.(string)
	if !ok {
		return time.Time{}, false
	}
	s = strings.TrimSpace(s)
	// Drop a trailing timezone name like "(BST)" from JS Date strings.
	if i := strings.Index(s, " ("); i > 0 {
		s = s[:i]
	}
	for _, l := range timeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// DecodeDocs decodes a JSON object or array of objects, preserving number
// formatting.
func DecodeDocs(raw []byte) ([]Doc, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("empty body")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if raw[0] == '[' {
		var docs []Doc
		if err := dec.Decode(&docs); err != nil {
			return nil, err
		}
		return docs, nil
	}
	var d Doc
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	if d == nil {
		return nil, errors.New("expected a JSON object")
	}
	return []Doc{d}, nil
}

// ---------------------------------------------------------------------------
// Fast paths used by alerts and reports

// Reading is a single glucose value.
type Reading struct {
	Time      int64   `json:"t"` // epoch ms
	Value     float64 `json:"v"` // mg/dL
	Direction string  `json:"d,omitempty"`
}

// SGVs returns CGM readings with from <= date < to, oldest first.
func (s *Store) SGVs(ctx context.Context, from, to time.Time) ([]Reading, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT date, sgv, COALESCE(direction, '') FROM entries
		WHERE type = 'sgv' AND is_valid = 1 AND sgv IS NOT NULL AND sgv > 0 AND date >= ? AND date < ?
		ORDER BY date ASC`, from.UnixMilli(), to.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reading
	for rows.Next() {
		var r Reading
		if err := rows.Scan(&r.Time, &r.Value, &r.Direction); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestSGV returns the most recent CGM reading.
func (s *Store) LatestSGV(ctx context.Context) (Reading, bool, error) {
	var r Reading
	err := s.db.QueryRowContext(ctx, `
		SELECT date, sgv, COALESCE(direction, '') FROM entries
		WHERE type = 'sgv' AND is_valid = 1 AND sgv IS NOT NULL AND sgv > 0
		ORDER BY date DESC LIMIT 1`).Scan(&r.Time, &r.Value, &r.Direction)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// ---------------------------------------------------------------------------
// Settings KV and audit log

// GetSetting implements settings.KV.
func (s *Store) GetSetting(ctx context.Context, key string) ([]byte, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(v), true, nil
}

// PutSetting implements settings.KV.
func (s *Store) PutSetting(ctx context.Context, key string, value []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, string(value))
	return err
}

// AuditEvent is one entry in the audit log.
type AuditEvent struct {
	At     int64  `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// Audit appends to the audit log. Errors are returned but callers usually
// just log them: auditing must never block care data.
func (s *Store) Audit(ctx context.Context, actor, action, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (at, actor, action, detail) VALUES (?, ?, ?, ?)`,
		time.Now().UnixMilli(), actor, action, detail)
	return err
}

// AuditLog returns the most recent audit events.
func (s *Store) AuditLog(ctx context.Context, limit int) ([]AuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT at, actor, action, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
