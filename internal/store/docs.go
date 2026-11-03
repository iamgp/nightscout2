package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Collection names, as used in Nightscout API paths.
const (
	Entries      = "entries"
	Treatments   = "treatments"
	DeviceStatus = "devicestatus"
	Profiles     = "profile"
	Food         = "food"
	ClientConfig = "settings" // Nightscout's v3 "settings" collection (AAPS config sync), not Tideline's own settings
)

// Collections lists every document collection.
var Collections = []string{Entries, Treatments, DeviceStatus, Profiles, Food, ClientConfig}

type fieldKind int

const (
	kindText fieldKind = iota
	kindNumber
	kindTime // stored as epoch ms; accepts ISO strings or epoch values
)

type column struct {
	name string
	kind fieldKind
}

type collection struct {
	table    string
	timeCol  string
	extra    []string // indexed columns besides id/time/doc, in insert order
	conflict string   // natural de-duplication key; "" means by id only
	fields   map[string]column
	// prepare normalises a document in place and returns its time and the
	// values for the extra columns.
	prepare func(d Doc, now time.Time) (time.Time, []any)
}

var metaFields = map[string]column{
	"_id":         {"id", kindText},
	"identifier":  {"id", kindText},
	"srvModified": {"srv_modified", kindNumber},
	"srvCreated":  {"srv_created", kindNumber},
}

func withMeta(f map[string]column) map[string]column {
	for k, v := range metaFields {
		f[k] = v
	}
	return f
}

// firstTime returns the first parseable timestamp among the given fields.
func firstTime(d Doc, now time.Time, keys ...string) time.Time {
	for _, k := range keys {
		if t, ok := ParseTime(d[k]); ok {
			return t
		}
	}
	return now
}

// setCreated normalises created_at to ISO and fills date (epoch ms), which
// API v3 clients use as the document time.
func setCreated(d Doc, t time.Time, keepString bool) {
	if _, ok := d["created_at"].(string); !ok || !keepString {
		d["created_at"] = isoMillis(t)
	}
	if _, ok := toFloat(d["date"]); !ok {
		d["date"] = t.UnixMilli()
	}
}

var collections = map[string]collection{
	Entries: {
		table: "entries", timeCol: "date",
		extra:    []string{"type", "sgv", "direction", "device"},
		conflict: "type, date",
		fields: withMeta(map[string]column{
			"type":       {"type", kindText},
			"date":       {"date", kindTime},
			"dateString": {"date", kindTime},
			"sysTime":    {"date", kindTime},
			"sgv":        {"sgv", kindNumber},
			"direction":  {"direction", kindText},
			"device":     {"device", kindText},
		}),
		prepare: func(d Doc, now time.Time) (time.Time, []any) {
			typ, _ := d["type"].(string)
			if typ == "" {
				typ = "sgv"
				d["type"] = typ
			}
			t := firstTime(d, now, "date", "dateString", "sysTime")
			d["date"] = t.UnixMilli()
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
			return t, []any{typ, sgv, dir, dev}
		},
	},
	Treatments: {
		table: "treatments", timeCol: "created_at",
		extra:    []string{"event_type"},
		conflict: "event_type, created_at",
		fields: withMeta(map[string]column{
			"eventType":  {"event_type", kindText},
			"created_at": {"created_at", kindTime},
			"date":       {"created_at", kindTime},
		}),
		prepare: func(d Doc, now time.Time) (time.Time, []any) {
			et, _ := d["eventType"].(string)
			if et == "" {
				et = "<none>"
				d["eventType"] = et
			}
			t := firstTime(d, now, "created_at", "timestamp", "date", "mills")
			setCreated(d, t, false)
			return t, []any{et}
		},
	},
	DeviceStatus: {
		table: "devicestatus", timeCol: "created_at",
		extra:    []string{"device"},
		conflict: "device, created_at",
		fields: withMeta(map[string]column{
			"created_at": {"created_at", kindTime},
			"date":       {"created_at", kindTime},
			"device":     {"device", kindText},
		}),
		prepare: func(d Doc, now time.Time) (time.Time, []any) {
			t := firstTime(d, now, "created_at", "date", "mills")
			setCreated(d, t, false)
			dev, _ := d["device"].(string)
			return t, []any{dev}
		},
	},
	Profiles: {
		table: "profile", timeCol: "created_at",
		fields: withMeta(map[string]column{
			"created_at": {"created_at", kindTime},
			"startDate":  {"created_at", kindTime},
			"date":       {"created_at", kindTime},
		}),
		prepare: func(d Doc, now time.Time) (time.Time, []any) {
			t := firstTime(d, now, "date", "created_at", "startDate", "mills")
			setCreated(d, t, true)
			return t, nil
		},
	},
	Food:         genericCollection("food"),
	ClientConfig: genericCollection("ns_settings"),
}

func genericCollection(table string) collection {
	return collection{
		table: table, timeCol: "created_at",
		fields: withMeta(map[string]column{
			"created_at": {"created_at", kindTime},
			"date":       {"created_at", kindTime},
		}),
		prepare: func(d Doc, now time.Time) (time.Time, []any) {
			t := firstTime(d, now, "date", "created_at", "mills")
			setCreated(d, t, true)
			return t, nil
		},
	}
}

func lookup(coll string) (collection, error) {
	c, ok := collections[coll]
	if !ok {
		return c, fmt.Errorf("unknown collection %q", coll)
	}
	return c, nil
}

// serverFields are managed by the server and never stored in the document body.
var serverFields = []string{"_id", "identifier", "srvModified", "srvCreated", "isValid"}

func cloneDoc(d Doc) Doc {
	c := make(Doc, len(d)+4)
	for k, v := range d {
		c[k] = v
	}
	return c
}

// clientID returns the ID the client supplied, if any.
func clientID(d Doc) string {
	for _, k := range []string{"identifier", "_id"} {
		if id, ok := d[k].(string); ok && id != "" && len(id) <= 128 {
			return id
		}
	}
	return ""
}

func stripServerFields(d Doc) {
	for _, k := range serverFields {
		delete(d, k)
	}
}

// decorate adds server metadata to a stored document for output.
func decorate(d Doc, id string, created, modified int64, valid bool) Doc {
	d["_id"] = id
	d["identifier"] = id
	d["srvCreated"] = created
	d["srvModified"] = modified
	if !valid {
		d["isValid"] = false
	}
	return d
}

func decodeStored(raw string) (Doc, error) {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var d Doc
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Writes

// Result describes one written document.
type Result struct {
	Doc          Doc
	Deduplicated bool // an existing document was matched and updated
}

// ErrConflict is returned when an update would collide with another document.
var ErrConflict = errors.New("document conflicts with an existing one")

// Upsert writes documents to a collection. A document matching an existing
// one, by client-supplied ID or by the collection's natural key (e.g. type +
// date for entries), replaces it: uploaders resend often, and Nightscout
// de-duplicates the same way.
func (s *Store) Upsert(ctx context.Context, coll string, docs []Doc) ([]Result, error) {
	c, err := lookup(coll)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	cols := append([]string{"id", c.timeCol}, c.extra...)
	cols = append(cols, "doc", "srv_created", "srv_modified", "is_valid")
	ph := strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ")
	set := []string{c.timeCol + " = excluded." + c.timeCol}
	for _, e := range c.extra {
		set = append(set, e+" = excluded."+e)
	}
	set = append(set, "doc = excluded.doc", "srv_modified = excluded.srv_modified", "is_valid = 1")
	target := c.conflict
	if target == "" {
		target = "id"
	}
	insert, err := tx.PrepareContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (%s) VALUES (%s) ON CONFLICT(%s) DO UPDATE SET %s RETURNING id, srv_created`,
		c.table, strings.Join(cols, ", "), ph, target, strings.Join(set, ", ")))
	if err != nil {
		return nil, err
	}
	defer insert.Close()

	out := make([]Result, 0, len(docs))
	for _, in := range docs {
		d := cloneDoc(in)
		id := clientID(d)
		stripServerFields(d)
		t, extra := c.prepare(d, now)
		raw, err := json.Marshal(d)
		if err != nil {
			return nil, err
		}
		mod := s.nextModified()

		// A client resending a document by its ID is an update, even if its
		// natural key changed.
		if id != "" && c.conflict != "" {
			var created int64
			err := tx.QueryRowContext(ctx, `SELECT srv_created FROM `+c.table+` WHERE id = ?`, id).Scan(&created)
			if err == nil {
				if err := updateRow(ctx, tx, c, id, t, extra, string(raw), mod); err != nil {
					return nil, err
				}
				out = append(out, Result{decorate(d, id, created, mod, true), true})
				continue
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		proposed := id
		if proposed == "" {
			proposed = NewID()
		}
		args := append([]any{proposed, t.UnixMilli()}, extra...)
		args = append(args, string(raw), now.UnixMilli(), mod, 1)
		var gotID string
		var created int64
		if err := insert.QueryRowContext(ctx, args...).Scan(&gotID, &created); err != nil {
			return nil, err
		}
		dedup := gotID != proposed || created != now.UnixMilli()
		out = append(out, Result{decorate(d, gotID, created, mod, true), dedup})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	docsOut := make([]Doc, len(out))
	for i, r := range out {
		docsOut[i] = r.Doc
	}
	s.emit(coll, docsOut)
	return out, nil
}

func updateRow(ctx context.Context, tx *sql.Tx, c collection, id string, t time.Time, extra []any, raw string, mod int64) error {
	set := []string{c.timeCol + " = ?"}
	args := []any{t.UnixMilli()}
	for i, e := range c.extra {
		set = append(set, e+" = ?")
		args = append(args, extra[i])
	}
	set = append(set, "doc = ?", "srv_modified = ?", "is_valid = 1")
	args = append(args, raw, mod, id)
	_, err := tx.ExecContext(ctx, `UPDATE `+c.table+` SET `+strings.Join(set, ", ")+` WHERE id = ?`, args...)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrConflict
	}
	return err
}

func docs(rs []Result) []Doc {
	out := make([]Doc, len(rs))
	for i, r := range rs {
		out[i] = r.Doc
	}
	return out
}

// InsertEntries upserts CGM and meter entries.
func (s *Store) InsertEntries(ctx context.Context, d []Doc) ([]Doc, error) {
	rs, err := s.Upsert(ctx, Entries, d)
	return docs(rs), err
}

// InsertTreatments upserts treatments (boluses, carbs, temp basals, notes…).
func (s *Store) InsertTreatments(ctx context.Context, d []Doc) ([]Doc, error) {
	rs, err := s.Upsert(ctx, Treatments, d)
	return docs(rs), err
}

// InsertDeviceStatus upserts device status records.
func (s *Store) InsertDeviceStatus(ctx context.Context, d []Doc) ([]Doc, error) {
	rs, err := s.Upsert(ctx, DeviceStatus, d)
	return docs(rs), err
}

// InsertProfiles upserts therapy profiles.
func (s *Store) InsertProfiles(ctx context.Context, d []Doc) ([]Doc, error) {
	rs, err := s.Upsert(ctx, Profiles, d)
	return docs(rs), err
}

// Replace overwrites a live document by ID. It returns false if no live
// document has that ID.
func (s *Store) Replace(ctx context.Context, coll, id string, in Doc) (Doc, bool, error) {
	c, err := lookup(coll)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var created int64
	err = tx.QueryRowContext(ctx, `SELECT srv_created FROM `+c.table+` WHERE id = ? AND is_valid = 1`, id).Scan(&created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	d := cloneDoc(in)
	stripServerFields(d)
	t, extra := c.prepare(d, time.Now())
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, false, err
	}
	mod := s.nextModified()
	if err := updateRow(ctx, tx, c, id, t, extra, string(raw), mod); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	d = decorate(d, id, created, mod, true)
	s.emit(coll, []Doc{d})
	return d, true, nil
}

// Patch merges fields into a live document. A null value removes a field.
func (s *Store) Patch(ctx context.Context, coll, id string, patch Doc) (Doc, bool, error) {
	cur, deleted, ok, err := s.GetAny(ctx, coll, id)
	if err != nil || !ok || deleted {
		return nil, false, err
	}
	for k, v := range patch {
		if v == nil {
			delete(cur, k)
		} else {
			cur[k] = v
		}
	}
	return s.Replace(ctx, coll, id, cur)
}

// Delete soft-deletes a document by ID. Returns whether a live document existed.
func (s *Store) Delete(ctx context.Context, coll, id string) (bool, error) {
	c, err := lookup(coll)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE `+c.table+` SET is_valid = 0, srv_modified = ? WHERE id = ? AND is_valid = 1`,
		s.nextModified(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		s.emit(coll, []Doc{{"_id": id, "identifier": id, "_deleted": true}})
	}
	return n > 0, nil
}

// ---------------------------------------------------------------------------
// Reads

// Filter is one query condition, e.g. date $gte 1700000000000.
type Filter struct {
	Field string
	Op    string // $eq $ne $gt $gte $lt $lte $in $nin
	Value string
}

// Query describes a read against a collection.
type Query struct {
	Filters []Filter
	Count   int    // max documents; 0 means the collection default
	Skip    int    // documents to skip
	Sort    string // field to sort by; "" means the collection's time field
	Asc     bool   // ascending (default newest first)
}

var findRe = regexp.MustCompile(`^find\[([^\]]+)\](?:\[(\$[a-z]+)\])?$`)

var validOps = map[string]bool{"$eq": true, "$ne": true, "$gt": true, "$gte": true, "$lt": true, "$lte": true, "$in": true, "$nin": true}

// ParseQuery parses Nightscout API v1's Mongo-style query string, e.g.
// find[date][$gte]=1700000000000&find[type]=sgv&count=288.
func ParseQuery(v url.Values) (Query, error) {
	var q Query
	for key, vals := range v {
		m := findRe.FindStringSubmatch(key)
		if m == nil {
			continue
		}
		op := m[2]
		if op == "" {
			op = "$eq"
		}
		if !validOps[op] {
			return q, fmt.Errorf("unsupported operator %s", op)
		}
		for _, val := range vals {
			q.Filters = append(q.Filters, Filter{Field: m[1], Op: op, Value: val})
		}
	}
	if c := v.Get("count"); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 {
			return q, fmt.Errorf("invalid count %q", c)
		}
		q.Count = n
	}
	return q, nil
}

// ParseQueryV3 parses Nightscout API v3 search parameters, e.g.
// date$gt=1700000000000&type=sgv&sort$desc=date&limit=100&skip=0.
func ParseQueryV3(v url.Values) (Query, error) {
	q := Query{Count: 100}
	reserved := map[string]bool{"limit": true, "skip": true, "sort": true, "sort$desc": true,
		"fields": true, "token": true, "secret": true, "now": true}
	for key, vals := range v {
		if reserved[key] {
			continue
		}
		field, op, _ := strings.Cut(key, "$")
		if op == "" {
			op = "eq"
		}
		op = "$" + op
		if op == "$re" {
			return q, errors.New("regular expression filters are not supported")
		}
		if !validOps[op] {
			return q, fmt.Errorf("unsupported operator %s", op)
		}
		for _, val := range vals {
			q.Filters = append(q.Filters, Filter{Field: field, Op: op, Value: val})
		}
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			return q, fmt.Errorf("invalid limit %q", l)
		}
		q.Count = n
	}
	if sk := v.Get("skip"); sk != "" {
		n, err := strconv.Atoi(sk)
		if err != nil || n < 0 {
			return q, fmt.Errorf("invalid skip %q", sk)
		}
		q.Skip = n
	}
	if s := v.Get("sort"); s != "" {
		q.Sort, q.Asc = s, true
	}
	if s := v.Get("sort$desc"); s != "" {
		q.Sort, q.Asc = s, false
	}
	return q, nil
}

var jsonFieldRe = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)

const maxCount = 100000

func (c collection) expr(field string) (string, fieldKind, error) {
	if col, ok := c.fields[field]; ok {
		return col.name, col.kind, nil
	}
	if jsonFieldRe.MatchString(field) {
		// Numeric if the value parses as one, text otherwise (see convert).
		return "json_extract(doc, '$." + field + "')", kindNumber, nil
	}
	return "", 0, fmt.Errorf("invalid field %q", field)
}

const selectCols = "id, doc, srv_created, srv_modified, is_valid"

func scanDocs(rows *sql.Rows) ([]Doc, error) {
	defer rows.Close()
	out := []Doc{}
	for rows.Next() {
		var id, raw string
		var created, modified int64
		var valid bool
		if err := rows.Scan(&id, &raw, &created, &modified, &valid); err != nil {
			return nil, err
		}
		d, err := decodeStored(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, decorate(d, id, created, modified, valid))
	}
	return out, rows.Err()
}

// Find runs a query against the live documents in a collection.
func (s *Store) Find(ctx context.Context, coll string, q Query) ([]Doc, error) {
	c, err := lookup(coll)
	if err != nil {
		return nil, err
	}
	where := []string{"is_valid = 1"}
	var args []any
	for _, f := range q.Filters {
		expr, kind, err := c.expr(f.Field)
		if err != nil {
			return nil, err
		}
		if f.Op == "$in" || f.Op == "$nin" {
			parts := strings.Split(f.Value, "|")
			ph := make([]string, len(parts))
			for i, p := range parts {
				ph[i] = "?"
				args = append(args, convert(kind, p))
			}
			not := ""
			if f.Op == "$nin" {
				not = "NOT "
			}
			where = append(where, fmt.Sprintf("%s %sIN (%s)", expr, not, strings.Join(ph, ",")))
			continue
		}
		sqlOp := map[string]string{"$eq": "=", "$ne": "!=", "$gt": ">", "$gte": ">=", "$lt": "<", "$lte": "<="}[f.Op]
		where = append(where, expr+" "+sqlOp+" ?")
		args = append(args, convert(kind, f.Value))
	}
	sortExpr := c.timeCol
	if q.Sort != "" {
		if sortExpr, _, err = c.expr(q.Sort); err != nil {
			return nil, err
		}
	}
	order := "DESC"
	if q.Asc {
		order = "ASC"
	}
	count := q.Count
	if count == 0 {
		count = 10
		if coll == Treatments {
			count = 100
		}
	}
	count = min(count, maxCount)
	sqlq := fmt.Sprintf("SELECT %s FROM %s WHERE %s ORDER BY %s %s, id %s LIMIT ? OFFSET ?",
		selectCols, c.table, strings.Join(where, " AND "), sortExpr, order, order)
	args = append(args, count, q.Skip)
	rows, err := s.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	return scanDocs(rows)
}

// Get returns a live document by ID.
func (s *Store) Get(ctx context.Context, coll, id string) (Doc, bool, error) {
	d, deleted, ok, err := s.GetAny(ctx, coll, id)
	if err != nil || !ok || deleted {
		return nil, false, err
	}
	return d, true, nil
}

// GetAny returns a document by ID, including deleted ones.
func (s *Store) GetAny(ctx context.Context, coll, id string) (d Doc, deleted, ok bool, err error) {
	c, err := lookup(coll)
	if err != nil {
		return nil, false, false, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectCols+` FROM `+c.table+` WHERE id = ?`, id)
	if err != nil {
		return nil, false, false, err
	}
	ds, err := scanDocs(rows)
	if err != nil || len(ds) == 0 {
		return nil, false, false, err
	}
	_, del := ds[0]["isValid"]
	return ds[0], del, true, nil
}

// History returns documents (including deleted ones, marked isValid=false)
// modified after since, oldest change first. This is how API v3 clients sync.
func (s *Store) History(ctx context.Context, coll string, since int64, limit int) ([]Doc, error) {
	c, err := lookup(coll)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+selectCols+` FROM `+c.table+`
		WHERE srv_modified > ? ORDER BY srv_modified ASC LIMIT ?`, since, min(limit, maxCount))
	if err != nil {
		return nil, err
	}
	return scanDocs(rows)
}

// LastModified returns the latest srv_modified per collection.
func (s *Store) LastModified(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, name := range Collections {
		c := collections[name]
		var m int64
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(srv_modified), 0) FROM `+c.table).Scan(&m); err != nil {
			return nil, err
		}
		out[name] = m
	}
	return out, nil
}

func convert(kind fieldKind, v string) any {
	switch kind {
	case kindTime:
		if t, ok := ParseTime(v); ok {
			return t.UnixMilli()
		}
		return v
	case kindNumber:
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
		return v
	}
	return v
}
