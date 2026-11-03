// Package store persists Tideline data in SQLite.
//
// Documents (entries, treatments, device status, profiles) are stored as the
// JSON the uploader sent, alongside a few indexed columns used for querying.
// Keeping the original document means Nightscout clients get back exactly the
// fields they wrote, including ones Tideline doesn't know about.
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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // SQLite driver (pure Go)
)

// Doc is a JSON document as exchanged with Nightscout clients.
type Doc = map[string]any

// Collection names.
const (
	Entries      = "entries"
	Treatments   = "treatments"
	DeviceStatus = "devicestatus"
	Profiles     = "profile"
)

// ChangeFunc is called after documents are written to a collection.
type ChangeFunc func(collection string, docs []Doc)

// Store is a SQLite-backed document store.
type Store struct {
	db *sql.DB

	mu        sync.RWMutex
	listeners []ChangeFunc
}

const schema = `
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

// Open opens (creating if needed) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = "file::memory:"
	} else {
		dsn = "file:" + path
	}
	dsn += "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serialises writes and keeps in-memory DBs coherent.
	// Tideline serves one person's data, so this is plenty.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
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

var objectIDRe = regexp.MustCompile(`^[0-9a-fA-F]{24}$`)

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

func takeID(d Doc) string {
	id, _ := d["_id"].(string)
	delete(d, "_id")
	if id != "" && len(id) <= 64 {
		return id
	}
	return NewID()
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
	return []Doc{d}, nil
}

func decodeDoc(id, raw string) (Doc, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var d Doc
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	d["_id"] = id
	return d, nil
}

// ---------------------------------------------------------------------------
// Writes

// InsertEntries upserts CGM/meter entries. An entry with the same type and
// timestamp as an existing one replaces it (uploaders frequently resend).
func (s *Store) InsertEntries(ctx context.Context, docs []Doc) ([]Doc, error) {
	now := time.Now()
	out := make([]Doc, 0, len(docs))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO entries (id, type, date, sgv, direction, device, doc, srv_created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(type, date) DO UPDATE SET
			sgv = excluded.sgv, direction = excluded.direction,
			device = excluded.device, doc = excluded.doc
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, in := range docs {
		d := cloneDoc(in)
		id := takeID(d)
		typ, _ := d["type"].(string)
		if typ == "" {
			typ = "sgv"
			d["type"] = typ
		}
		t, ok := ParseTime(d["date"])
		if !ok {
			if t, ok = ParseTime(d["dateString"]); !ok {
				if t, ok = ParseTime(d["sysTime"]); !ok {
					t = now
				}
			}
		}
		ms := t.UnixMilli()
		d["date"] = ms
		if _, ok := d["dateString"].(string); !ok {
			d["dateString"] = isoMillis(t)
		}
		if _, ok := d["sysTime"].(string); !ok {
			d["sysTime"] = d["dateString"]
		}
		var sgv any
		if f, ok := toFloat(d["sgv"]); ok {
			sgv = f
		} else if typ == "mbg" {
			if f, ok := toFloat(d["mbg"]); ok {
				sgv = f
			}
		}
		dir, _ := d["direction"].(string)
		dev, _ := d["device"].(string)
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		var gotID string
		if err := stmt.QueryRowContext(ctx, id, typ, ms, sgv, dir, dev, string(raw), now.UnixMilli()).Scan(&gotID); err != nil {
			return nil, err
		}
		d["_id"] = gotID
		out = append(out, d)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.emit(Entries, out)
	return out, nil
}

// InsertTreatments upserts treatments (boluses, carbs, temp basals, notes…).
// Treatments are de-duplicated on eventType + created_at, as in Nightscout.
func (s *Store) InsertTreatments(ctx context.Context, docs []Doc) ([]Doc, error) {
	now := time.Now()
	out := make([]Doc, 0, len(docs))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO treatments (id, event_type, created_at, doc, srv_created)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(event_type, created_at) DO UPDATE SET doc = excluded.doc
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, in := range docs {
		d := cloneDoc(in)
		id := takeID(d)
		et, _ := d["eventType"].(string)
		if et == "" {
			et = "<none>"
			d["eventType"] = et
		}
		t, ok := ParseTime(d["created_at"])
		if !ok {
			if t, ok = ParseTime(d["timestamp"]); !ok {
				if t, ok = ParseTime(d["date"]); !ok {
					t = now
				}
			}
		}
		d["created_at"] = isoMillis(t)
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		var gotID string
		if err := stmt.QueryRowContext(ctx, id, et, t.UnixMilli(), string(raw), now.UnixMilli()).Scan(&gotID); err != nil {
			return nil, err
		}
		d["_id"] = gotID
		out = append(out, d)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.emit(Treatments, out)
	return out, nil
}

// UpdateTreatment replaces a treatment by ID. Returns false if it doesn't exist.
func (s *Store) UpdateTreatment(ctx context.Context, d Doc) (Doc, bool, error) {
	d = cloneDoc(d)
	id, _ := d["_id"].(string)
	if id == "" {
		return nil, false, errors.New("_id is required")
	}
	delete(d, "_id")
	et, _ := d["eventType"].(string)
	if et == "" {
		et = "<none>"
		d["eventType"] = et
	}
	t, ok := ParseTime(d["created_at"])
	if !ok {
		return nil, false, errors.New("created_at is required")
	}
	d["created_at"] = isoMillis(t)
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE treatments SET event_type = ?, created_at = ?, doc = ? WHERE id = ?`,
		et, t.UnixMilli(), string(raw), id)
	if err != nil {
		return nil, false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil, false, nil
	}
	d["_id"] = id
	s.emit(Treatments, []Doc{d})
	return d, true, nil
}

// InsertDeviceStatus upserts device status records (pump, loop, uploader battery…).
func (s *Store) InsertDeviceStatus(ctx context.Context, docs []Doc) ([]Doc, error) {
	now := time.Now()
	out := make([]Doc, 0, len(docs))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO devicestatus (id, created_at, device, doc, srv_created)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(device, created_at) DO UPDATE SET doc = excluded.doc
		RETURNING id`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for _, in := range docs {
		d := cloneDoc(in)
		id := takeID(d)
		t, ok := ParseTime(d["created_at"])
		if !ok {
			t = now
		}
		d["created_at"] = isoMillis(t)
		dev, _ := d["device"].(string)
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		var gotID string
		if err := stmt.QueryRowContext(ctx, id, t.UnixMilli(), dev, string(raw), now.UnixMilli()).Scan(&gotID); err != nil {
			return nil, err
		}
		d["_id"] = gotID
		out = append(out, d)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.emit(DeviceStatus, out)
	return out, nil
}

// InsertProfiles stores therapy profiles. Profiles with an existing _id are replaced.
func (s *Store) InsertProfiles(ctx context.Context, docs []Doc) ([]Doc, error) {
	now := time.Now()
	out := make([]Doc, 0, len(docs))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for _, in := range docs {
		d := cloneDoc(in)
		id := takeID(d)
		t, ok := ParseTime(d["created_at"])
		if !ok {
			if t, ok = ParseTime(d["startDate"]); !ok {
				t = now
			}
		}
		if _, ok := d["created_at"].(string); !ok {
			d["created_at"] = isoMillis(t)
		}
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO profile (id, created_at, doc, srv_created) VALUES (?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET created_at = excluded.created_at, doc = excluded.doc`,
			id, t.UnixMilli(), string(raw), now.UnixMilli()); err != nil {
			return nil, err
		}
		d["_id"] = id
		out = append(out, d)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.emit(Profiles, out)
	return out, nil
}

// Delete removes a document by ID from a collection. Returns whether it existed.
func (s *Store) Delete(ctx context.Context, coll, id string) (bool, error) {
	c, ok := collections[coll]
	if !ok {
		return false, fmt.Errorf("unknown collection %q", coll)
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM `+c.table+` WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.emit(coll, []Doc{{"_id": id, "_deleted": true}})
	}
	return n > 0, nil
}

func cloneDoc(d Doc) Doc {
	c := make(Doc, len(d)+3)
	for k, v := range d {
		c[k] = v
	}
	return c
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
		WHERE type = 'sgv' AND sgv IS NOT NULL AND sgv > 0 AND date >= ? AND date < ?
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
		WHERE type = 'sgv' AND sgv IS NOT NULL AND sgv > 0
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
