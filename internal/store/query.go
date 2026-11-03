package store

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

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
	table   string
	timeCol string
	fields  map[string]column
}

var collections = map[string]collection{
	Entries: {
		table:   "entries",
		timeCol: "date",
		fields: map[string]column{
			"_id":        {"id", kindText},
			"type":       {"type", kindText},
			"date":       {"date", kindTime},
			"dateString": {"date", kindTime},
			"sysTime":    {"date", kindTime},
			"sgv":        {"sgv", kindNumber},
			"direction":  {"direction", kindText},
			"device":     {"device", kindText},
		},
	},
	Treatments: {
		table:   "treatments",
		timeCol: "created_at",
		fields: map[string]column{
			"_id":        {"id", kindText},
			"eventType":  {"event_type", kindText},
			"created_at": {"created_at", kindTime},
		},
	},
	DeviceStatus: {
		table:   "devicestatus",
		timeCol: "created_at",
		fields: map[string]column{
			"_id":        {"id", kindText},
			"created_at": {"created_at", kindTime},
			"device":     {"device", kindText},
		},
	},
	Profiles: {
		table:   "profile",
		timeCol: "created_at",
		fields: map[string]column{
			"_id":        {"id", kindText},
			"created_at": {"created_at", kindTime},
			"startDate":  {"created_at", kindTime},
		},
	},
}

// Filter is one condition from a Nightscout-style find[field][$op]=value query.
type Filter struct {
	Field string
	Op    string // $eq $ne $gt $gte $lt $lte $in $nin
	Value string
}

// Query describes a read against a collection.
type Query struct {
	Filters []Filter
	Count   int  // max documents; 0 means the collection default
	Asc     bool // oldest first (default newest first)
}

var findRe = regexp.MustCompile(`^find\[([^\]]+)\](?:\[(\$[a-z]+)\])?$`)

// ParseQuery parses Nightscout's Mongo-style query string, e.g.
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
		switch op {
		case "$eq", "$ne", "$gt", "$gte", "$lt", "$lte", "$in", "$nin":
		default:
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

var jsonFieldRe = regexp.MustCompile(`^[A-Za-z0-9_]+(\.[A-Za-z0-9_]+)*$`)

const maxCount = 100000

// Find runs a query against a collection.
func (s *Store) Find(ctx context.Context, coll string, q Query) ([]Doc, error) {
	c, ok := collections[coll]
	if !ok {
		return nil, fmt.Errorf("unknown collection %q", coll)
	}
	var where []string
	var args []any
	for _, f := range q.Filters {
		expr, kind := "", kindText
		if col, ok := c.fields[f.Field]; ok {
			expr, kind = col.name, col.kind
		} else if jsonFieldRe.MatchString(f.Field) {
			expr = "json_extract(doc, '$." + f.Field + "')"
			kind = kindNumber // numeric if it parses, text otherwise (see convert)
		} else {
			return nil, fmt.Errorf("invalid field %q", f.Field)
		}
		sqlOp := map[string]string{"$eq": "=", "$ne": "!=", "$gt": ">", "$gte": ">=", "$lt": "<", "$lte": "<="}[f.Op]
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
		where = append(where, expr+" "+sqlOp+" ?")
		args = append(args, convert(kind, f.Value))
	}
	sqlq := "SELECT id, doc FROM " + c.table
	if len(where) > 0 {
		sqlq += " WHERE " + strings.Join(where, " AND ")
	}
	order := "DESC"
	if q.Asc {
		order = "ASC"
	}
	sqlq += " ORDER BY " + c.timeCol + " " + order
	count := q.Count
	if count == 0 {
		count = 10
		if coll == Treatments {
			count = 100
		}
	}
	if count > maxCount {
		count = maxCount
	}
	sqlq += " LIMIT ?"
	args = append(args, count)

	rows, err := s.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Doc{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		d, err := decodeDoc(id, raw)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Get returns a single document by ID.
func (s *Store) Get(ctx context.Context, coll, id string) (Doc, bool, error) {
	docs, err := s.Find(ctx, coll, Query{Filters: []Filter{{Field: "_id", Op: "$eq", Value: id}}, Count: 1})
	if err != nil || len(docs) == 0 {
		return nil, false, err
	}
	return docs[0], true, nil
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
