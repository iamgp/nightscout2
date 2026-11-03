package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigratesV1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	// Build a database exactly as Tideline 1.0 left it.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(baseSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO entries (id, type, date, sgv, direction, device, doc, srv_created)
		VALUES ('old1', 'sgv', 1767225600000, 120, 'Flat', 'xDrip', '{"type":"sgv","sgv":120,"date":1767225600000}', 1767225601000)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open/migrate: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	docs, err := s.Find(ctx, Entries, Query{})
	if err != nil || len(docs) != 1 {
		t.Fatalf("find after migration: %v %v", docs, err)
	}
	if docs[0]["srvModified"].(int64) != 1767225601000 || docs[0]["identifier"] != "old1" {
		t.Fatalf("metadata not backfilled: %v", docs[0])
	}
	if _, err := s.Upsert(ctx, Food, []Doc{{"name": "Toast"}}); err != nil {
		t.Fatalf("new collection: %v", err)
	}
	// srvModified keeps increasing past existing data.
	rs, _ := s.Upsert(ctx, Entries, []Doc{{"sgv": 130, "date": 1767225900000}})
	if rs[0].Doc["srvModified"].(int64) <= 1767225601000 {
		t.Fatalf("srvModified went backwards: %v", rs[0].Doc)
	}
	// Re-opening is a no-op.
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen: %v", err)
	}
}

func TestSrvModifiedStrictlyIncreases(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var batch []Doc
	for i := 0; i < 50; i++ {
		batch = append(batch, Doc{"sgv": 100, "date": int64(1767225600000 + i*300000)})
	}
	rs, err := s.Upsert(context.Background(), Entries, batch)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(rs); i++ {
		if rs[i].Doc["srvModified"].(int64) <= rs[i-1].Doc["srvModified"].(int64) {
			t.Fatalf("not increasing at %d", i)
		}
	}
	// Paging through history in small pages sees every document exactly once.
	seen := map[string]bool{}
	var since int64
	for {
		page, err := s.History(context.Background(), Entries, since, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		for _, d := range page {
			id := d["identifier"].(string)
			if seen[id] {
				t.Fatalf("duplicate %s", id)
			}
			seen[id] = true
			since = d["srvModified"].(int64)
		}
	}
	if len(seen) != 50 {
		t.Fatalf("saw %d of 50", len(seen))
	}
}

func TestClientIDUpdateCanMoveNaturalKey(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	rs, _ := s.Upsert(ctx, Treatments, []Doc{{"identifier": "abc", "eventType": "Note", "created_at": "2026-01-01T10:00:00Z"}})
	if rs[0].Deduplicated {
		t.Fatal("first insert flagged as dedup")
	}
	rs, err = s.Upsert(ctx, Treatments, []Doc{{"identifier": "abc", "eventType": "Note", "created_at": "2026-01-01T11:00:00Z"}})
	if err != nil || !rs[0].Deduplicated {
		t.Fatalf("resend by id: %v %v", rs, err)
	}
	all, _ := s.Find(ctx, Treatments, Query{})
	if len(all) != 1 || all[0]["created_at"] != "2026-01-01T11:00:00.000Z" {
		t.Fatalf("expected one moved treatment: %v", all)
	}
}
