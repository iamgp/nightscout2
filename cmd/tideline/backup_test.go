package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLitestreamConfig(t *testing.T) {
	raw, err := litestreamConfig("/data/tideline.db", "/opt/my apps/tideline", backupConfig{
		URL: "s3://tideline-backups/tideline", Endpoint: "https://abc123.r2.cloudflarestorage.com", Retention: "168h",
	})
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		DBs []struct {
			Path    string            `json:"path"`
			Restore bool              `json:"restore-if-db-not-exists"`
			Replica map[string]string `json:"replica"`
		} `json:"dbs"`
		Snapshot map[string]string `json:"snapshot"`
		Exec     string            `json:"exec"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	db := cfg.DBs[0]
	if db.Path != "/data/tideline.db" || !db.Restore || db.Replica["url"] != "s3://tideline-backups/tideline" ||
		db.Replica["endpoint"] != "https://abc123.r2.cloudflarestorage.com" {
		t.Fatalf("db: %+v", db)
	}
	if cfg.Snapshot["retention"] != "168h" || cfg.Exec != `'/opt/my apps/tideline' serve` {
		t.Fatalf("cfg: %+v", cfg)
	}
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "access-key") {
		t.Fatal("credentials must not be written to the config file")
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("s3://key:secret@host/bucket"); got != "s3://…@host/bucket" {
		t.Fatal(got)
	}
	if got := redactURL("s3://bucket/path"); got != "s3://bucket/path" {
		t.Fatal(got)
	}
}
