package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Continuous backups use Litestream (https://litestream.io), which streams
// every change to object storage such as Cloudflare R2 or S3. Tideline runs
// under it: Litestream restores the database if it's missing (for example
// after a lost volume), then starts `tideline serve` and replicates until it
// exits. On shutdown Litestream stops Tideline first and syncs the final
// changes.

const litestreamChildEnv = "TIDELINE_LITESTREAM_CHILD"

type backupConfig struct {
	URL       string // replica URL, e.g. s3://bucket/tideline or file:///backups/tideline
	Endpoint  string // S3-compatible endpoint, e.g. https://<account>.r2.cloudflarestorage.com
	Retention string // how far back point-in-time restores can go
}

func backupFromEnv() (backupConfig, bool) {
	c := backupConfig{
		URL:       env("TIDELINE_BACKUP_URL"),
		Endpoint:  env("TIDELINE_BACKUP_ENDPOINT"),
		Retention: firstNonEmpty(env("TIDELINE_BACKUP_RETENTION"), "168h"),
	}
	return c, c.URL != ""
}

// litestreamConfig renders the Litestream config. It is JSON, which YAML
// accepts, so every value is safely quoted; credentials stay in the
// environment and never touch disk.
func litestreamConfig(dbPath, self string, c backupConfig) ([]byte, error) {
	replica := map[string]any{"url": c.URL}
	if c.Endpoint != "" {
		replica["endpoint"] = c.Endpoint
	}
	cfg := map[string]any{
		"dbs": []any{map[string]any{
			"path":                     dbPath,
			"restore-if-db-not-exists": true,
			"replica":                  replica,
		}},
		// A daily snapshot plus the continuous change log gives restores to
		// any point within the retention window.
		"snapshot": map[string]any{"interval": "24h", "retention": c.Retention},
		"exec":     shellQuote(self) + " serve",
	}
	return json.MarshalIndent(cfg, "", "  ")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// findLitestream locates the Litestream binary.
func findLitestream() (string, error) {
	if p := env("TIDELINE_LITESTREAM_BIN"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("litestream"); err == nil {
		return p, nil
	}
	for _, p := range []string{"/usr/local/bin/litestream", "/litestream"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("backups are configured (TIDELINE_BACKUP_URL) but the litestream binary was not found; " +
		"use the Docker image, install Litestream, or set TIDELINE_LITESTREAM_BIN")
}

// runUnderLitestream replaces this process with Litestream supervising
// `tideline serve`, if backups are configured. It returns (false, nil) when
// backups are off or this process is already the supervised child.
func runUnderLitestream(log *slog.Logger) (bool, error) {
	c, ok := backupFromEnv()
	if !ok || os.Getenv(litestreamChildEnv) == "1" {
		return false, nil
	}
	bin, err := findLitestream()
	if err != nil {
		return false, err
	}
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	db, err := filepath.Abs(dbPath())
	if err != nil {
		return false, err
	}
	cfg, err := litestreamConfig(db, self, c)
	if err != nil {
		return false, err
	}
	cfgPath := filepath.Join(filepath.Dir(db), ".litestream.yml")
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return false, fmt.Errorf("write litestream config: %w", err)
	}

	envv := append(os.Environ(), litestreamChildEnv+"=1")
	// Friendlier names for the credentials Litestream reads.
	if v := env("TIDELINE_BACKUP_ACCESS_KEY_ID"); v != "" {
		envv = append(envv, "LITESTREAM_ACCESS_KEY_ID="+v)
	}
	if v := env("TIDELINE_BACKUP_SECRET_ACCESS_KEY"); v != "" {
		envv = append(envv, "LITESTREAM_SECRET_ACCESS_KEY="+v)
	}
	log.Info("starting continuous backups", "replica", redactURL(c.URL), "retention", c.Retention)
	// -no-expand-env: values are already literal, so a "$" in a path stays a "$".
	args := []string{bin, "replicate", "-config", cfgPath, "-no-expand-env"}
	return true, execReplace(bin, args, envv)
}

// redactURL drops any credentials embedded in a URL before logging it.
func redactURL(u string) string {
	if i := strings.Index(u, "@"); i > 0 {
		if j := strings.Index(u, "://"); j > 0 && j < i {
			return u[:j+3] + "…" + u[i:]
		}
	}
	return u
}
