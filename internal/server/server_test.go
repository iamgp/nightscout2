package server

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/iamgp/tideline/internal/auth"
	"github.com/iamgp/tideline/internal/importer"
	"github.com/iamgp/tideline/internal/settings"
	"github.com/iamgp/tideline/internal/store"
)

const secret = "correct-horse-battery"

func sha(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

type env struct {
	t   *testing.T
	ts  *httptest.Server
	st  *store.Store
	srv *Server
}

func newEnv(t *testing.T, publicRead bool) *env {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sm, err := settings.NewManager(context.Background(), st, settings.Default())
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, auth.New(secret, st, publicRead), sm, Options{Version: "test"})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &env{t: t, ts: ts, st: st, srv: srv}
}

// do performs a request. hdr may set "api-secret" or "Authorization".
func (e *env) do(method, path string, body any, hdr map[string]string) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			raw, _ := json.Marshal(b)
			rd = strings.NewReader(string(raw))
		}
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

var admin = map[string]string{"api-secret": sha(secret)}

func decode[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return v
}

func TestStatusIsPublic(t *testing.T) {
	e := newEnv(t, false)
	code, raw := e.do("GET", "/api/v1/status.json", nil, nil)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	st := decode[map[string]any](t, raw)
	if st["status"] != "ok" || st["apiEnabled"] != true {
		t.Fatalf("unexpected status: %s", raw)
	}
	th := st["settings"].(map[string]any)["thresholds"].(map[string]any)
	if th["bgTargetBottom"].(float64) != 70 || th["bgHigh"].(float64) != 250 {
		t.Fatalf("thresholds: %v", th)
	}
}

func TestPrivateByDefault(t *testing.T) {
	e := newEnv(t, false)
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, nil); code != 401 {
		t.Fatalf("anonymous read: got %d, want 401", code)
	}
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, map[string]string{"api-secret": "nope"}); code != 401 {
		t.Fatalf("bad secret: got %d, want 401", code)
	}
	if code, _ := e.do("POST", "/api/v1/entries", `[]`, nil); code != 401 {
		t.Fatalf("anonymous write: got %d", code)
	}
}

func TestPublicReadAllowsReadNotWrite(t *testing.T) {
	e := newEnv(t, true)
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, nil); code != 200 {
		t.Fatalf("public read: %d", code)
	}
	if code, _ := e.do("POST", "/api/v1/entries", `[{"sgv":100}]`, nil); code != 403 {
		t.Fatalf("public write: got %d, want 403", code)
	}
}

func TestEntriesRoundTripLikeXDrip(t *testing.T) {
	e := newEnv(t, false)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	var batch []map[string]any
	for i := 0; i < 6; i++ {
		batch = append(batch, map[string]any{
			"type": "sgv", "sgv": 100 + i*5, "direction": "FortyFiveUp",
			"date": base + int64(i)*300000, "device": "xDrip-DexcomG7", "noise": 1,
		})
	}
	code, raw := e.do("POST", "/api/v1/entries", batch, admin)
	if code != 200 {
		t.Fatalf("post: %d %s", code, raw)
	}
	posted := decode[[]map[string]any](t, raw)
	if len(posted) != 6 || posted[0]["_id"] == "" || posted[0]["dateString"] != "2026-01-01T12:00:00.000Z" {
		t.Fatalf("posted: %s", raw)
	}

	// Resending is idempotent and keeps the original _id.
	_, raw = e.do("POST", "/api/v1/entries.json", batch[:1], admin)
	again := decode[[]map[string]any](t, raw)
	if again[0]["_id"] != posted[0]["_id"] {
		t.Fatalf("dedupe changed id: %v vs %v", again[0]["_id"], posted[0]["_id"])
	}

	_, raw = e.do("GET", "/api/v1/entries.json?count=3", nil, admin)
	got := decode[[]map[string]any](t, raw)
	if len(got) != 3 || got[0]["sgv"].(float64) != 125 || got[2]["sgv"].(float64) != 115 {
		t.Fatalf("newest-first count=3: %s", raw)
	}
	if got[0]["noise"].(float64) != 1 || got[0]["device"] != "xDrip-DexcomG7" {
		t.Fatalf("extra fields not preserved: %v", got[0])
	}

	q := url.Values{"find[date][$gte]": {fmt.Sprint(base + 600000)}, "find[date][$lt]": {fmt.Sprint(base + 1200000)}, "count": {"100"}}
	_, raw = e.do("GET", "/api/v1/entries/sgv.json?"+q.Encode(), nil, admin)
	if got := decode[[]map[string]any](t, raw); len(got) != 2 {
		t.Fatalf("date range: %s", raw)
	}

	q = url.Values{"find[dateString][$gte]": {"2026-01-01T12:20:00Z"}}
	_, raw = e.do("GET", "/api/v1/entries.json?"+q.Encode(), nil, admin)
	if got := decode[[]map[string]any](t, raw); len(got) != 2 {
		t.Fatalf("dateString filter: %s", raw)
	}

	_, raw = e.do("GET", "/api/v1/entries/current.json", nil, admin)
	if got := decode[[]map[string]any](t, raw); len(got) != 1 || got[0]["sgv"].(float64) != 125 {
		t.Fatalf("current: %s", raw)
	}

	// Unknown fields are queried through the stored document.
	q = url.Values{"find[noise]": {"1"}, "count": {"100"}}
	_, raw = e.do("GET", "/api/v1/entries.json?"+q.Encode(), nil, admin)
	if got := decode[[]map[string]any](t, raw); len(got) != 6 {
		t.Fatalf("json field filter: %d", len(got))
	}

	id := posted[0]["_id"].(string)
	if code, _ := e.do("DELETE", "/api/v1/entries/"+id, nil, admin); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	_, raw = e.do("GET", "/api/v1/entries.json?count=100", nil, admin)
	if got := decode[[]map[string]any](t, raw); len(got) != 5 {
		t.Fatalf("after delete: %d", len(got))
	}
}

func TestTreatmentsLikeLoop(t *testing.T) {
	e := newEnv(t, false)
	code, raw := e.do("POST", "/api/v1/treatments", []map[string]any{
		{"eventType": "Correction Bolus", "insulin": 1.5, "created_at": "2026-01-01T08:00:00Z", "enteredBy": "loop://iPhone"},
		{"eventType": "Carb Correction", "carbs": 30, "absorptionTime": 180, "created_at": "2026-01-01T08:05:00.000Z"},
		{"eventType": "Temp Basal", "rate": 0.4, "duration": 30, "created_at": "2026-01-01T09:00:00Z"},
	}, admin)
	if code != 200 {
		t.Fatalf("post: %d %s", code, raw)
	}
	q := url.Values{"find[created_at][$gte]": {"2026-01-01T08:01:00Z"}, "count": {"10"}}
	_, raw = e.do("GET", "/api/v1/treatments.json?"+q.Encode(), nil, admin)
	got := decode[[]map[string]any](t, raw)
	if len(got) != 2 || got[0]["eventType"] != "Temp Basal" {
		t.Fatalf("filter: %s", raw)
	}
	q = url.Values{"find[eventType]": {"Correction Bolus"}}
	_, raw = e.do("GET", "/api/v1/treatments.json?"+q.Encode(), nil, admin)
	got = decode[[]map[string]any](t, raw)
	if len(got) != 1 || got[0]["insulin"].(float64) != 1.5 || got[0]["enteredBy"] != "loop://iPhone" {
		t.Fatalf("eventType filter: %s", raw)
	}

	upd := got[0]
	upd["insulin"] = 2.0
	code, raw = e.do("PUT", "/api/v1/treatments", upd, admin)
	if code != 200 || decode[map[string]any](t, raw)["insulin"].(float64) != 2 {
		t.Fatalf("put: %d %s", code, raw)
	}
	if code, _ := e.do("DELETE", "/api/v1/treatments/"+upd["_id"].(string), nil, admin); code != 200 {
		t.Fatal("delete failed")
	}
	_, raw = e.do("GET", "/api/v1/treatments.json", nil, admin)
	if got := decode[[]map[string]any](t, raw); len(got) != 2 {
		t.Fatalf("after delete: %s", raw)
	}
}

func TestDeviceStatusAndProfile(t *testing.T) {
	e := newEnv(t, false)
	e.do("POST", "/api/v1/devicestatus", map[string]any{
		"device": "loop://iPhone", "created_at": "2026-01-01T10:00:00Z",
		"loop": map[string]any{"iob": map[string]any{"iob": 1.2}}, "pump": map[string]any{"reservoir": 87},
	}, admin)
	_, raw := e.do("GET", "/api/v1/devicestatus.json?count=1", nil, admin)
	got := decode[[]map[string]any](t, raw)
	if len(got) != 1 || got[0]["pump"].(map[string]any)["reservoir"].(float64) != 87 {
		t.Fatalf("devicestatus: %s", raw)
	}
	e.do("POST", "/api/v1/profile", map[string]any{"defaultProfile": "Default", "startDate": "2026-01-01T00:00:00Z",
		"store": map[string]any{"Default": map[string]any{"dia": 6}}}, admin)
	code, raw := e.do("GET", "/api/v1/profile/current", nil, admin)
	if code != 200 || decode[map[string]any](t, raw)["defaultProfile"] != "Default" {
		t.Fatalf("profile: %d %s", code, raw)
	}
}

func TestTokensAndRoles(t *testing.T) {
	e := newEnv(t, false)
	code, raw := e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "School nurse", "role": "reader", "expiresInDays": 30}, admin)
	if code != 201 {
		t.Fatalf("create: %d %s", code, raw)
	}
	created := decode[map[string]any](t, raw)
	reader := created["token"].(string)
	if !strings.HasPrefix(reader, "school-nurse-") {
		t.Fatalf("token format: %s", reader)
	}
	rh := map[string]string{"Authorization": "Bearer " + reader}

	if code, _ := e.do("GET", "/api/v1/entries.json?token="+reader, nil, nil); code != 200 {
		t.Fatalf("reader via ?token: %d", code)
	}
	if code, _ := e.do("POST", "/api/v1/entries", `[{"sgv":100}]`, rh); code != 403 {
		t.Fatalf("reader write: %d", code)
	}
	if code, _ := e.do("GET", "/api/tl/v1/tokens", nil, rh); code != 403 {
		t.Fatalf("reader list tokens: %d", code)
	}
	_, raw = e.do("GET", "/api/v1/verifyauth", nil, rh)
	va := decode[map[string]any](t, raw)["message"].(map[string]any)
	if va["canRead"] != true || va["canWrite"] != false {
		t.Fatalf("verifyauth: %v", va)
	}

	_, raw = e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "xDrip", "role": "uploader"}, admin)
	up := decode[map[string]any](t, raw)["token"].(string)
	if code, _ := e.do("POST", "/api/v1/entries?token="+up, `{"sgv":101,"date":1767225600000}`, nil); code != 200 {
		t.Fatalf("uploader write: %d", code)
	}
	if code, _ := e.do("PUT", "/api/tl/v1/settings", settings.Default(), map[string]string{"Authorization": "Bearer " + up}); code != 403 {
		t.Fatalf("uploader settings: %d", code)
	}

	id := created["info"].(map[string]any)["id"].(string)
	if code, _ := e.do("DELETE", "/api/tl/v1/tokens/"+id, nil, admin); code != 200 {
		t.Fatal("revoke failed")
	}
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, rh); code != 401 {
		t.Fatalf("revoked token: %d", code)
	}
	_, raw = e.do("GET", "/api/tl/v1/audit", nil, admin)
	if !strings.Contains(string(raw), "revoke token") || !strings.Contains(string(raw), "School nurse") {
		t.Fatalf("audit: %s", raw)
	}
}

func TestSettingsValidationAndRedaction(t *testing.T) {
	e := newEnv(t, false)
	s := settings.Default()
	s.Thresholds.Low = 200
	if code, _ := e.do("PUT", "/api/tl/v1/settings", s, admin); code != 400 {
		t.Fatalf("invalid thresholds accepted: %d", code)
	}
	s = settings.Default()
	s.Units = settings.UnitsMmol
	s.Notifiers = []settings.Notifier{{Name: "Phone", Type: "ntfy", URL: "https://ntfy.sh/x", Token: "tk_secret", MinLevel: "warn", Enabled: true}}
	code, raw := e.do("PUT", "/api/tl/v1/settings", s, admin)
	if code != 200 {
		t.Fatalf("put: %d %s", code, raw)
	}
	_, raw = e.do("GET", "/api/v1/status.json", nil, nil)
	if !strings.Contains(string(raw), `"units":"mmol"`) {
		t.Fatalf("status units: %s", raw)
	}
	_, raw = e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "f", "role": "reader"}, admin)
	rd := decode[map[string]any](t, raw)["token"].(string)
	_, raw = e.do("GET", "/api/tl/v1/settings?token="+rd, nil, nil)
	if strings.Contains(string(raw), "tk_secret") {
		t.Fatalf("notifier secret leaked to reader: %s", raw)
	}
}

func TestPebble(t *testing.T) {
	e := newEnv(t, false)
	now := time.Now().UnixMilli()
	e.do("POST", "/api/v1/entries", []map[string]any{
		{"sgv": 120, "date": now - 300000, "direction": "Flat"},
		{"sgv": 126, "date": now, "direction": "FortyFiveUp"},
	}, admin)
	_, raw := e.do("GET", "/pebble?units=mmol", nil, admin)
	p := decode[map[string]any](t, raw)
	bg := p["bgs"].([]any)[0].(map[string]any)
	if bg["sgv"] != "7.0" || bg["bgdelta"] != "0.3" || bg["trend"].(float64) != 3 {
		t.Fatalf("pebble: %s", raw)
	}
}

func TestTimelineReportAndAlerts(t *testing.T) {
	e := newEnv(t, false)
	now := time.Now()
	var batch []map[string]any
	for i := 0; i < 12; i++ {
		batch = append(batch, map[string]any{"sgv": 75 - i, "date": now.Add(-time.Duration(11-i) * 5 * time.Minute).UnixMilli()})
	}
	e.do("POST", "/api/v1/entries", batch, admin)
	e.srv.Alerts().Evaluate(context.Background())

	_, raw := e.do("GET", "/api/tl/v1/timeline?hours=3", nil, admin)
	tl := decode[map[string]any](t, raw)
	if n := len(tl["readings"].([]any)); n != 12 {
		t.Fatalf("timeline readings: %d", n)
	}
	al := tl["alerts"].([]any)
	if len(al) != 1 || al[0].(map[string]any)["kind"] != "low" {
		t.Fatalf("expected low alert: %s", raw)
	}

	code, raw := e.do("POST", "/api/tl/v1/alerts/low/snooze", map[string]any{"minutes": 30}, admin)
	if code != 200 || !strings.Contains(string(raw), "snoozedUntil") {
		t.Fatalf("snooze: %d %s", code, raw)
	}

	_, raw = e.do("GET", "/api/tl/v1/report?days=1&tz=Europe/London", nil, admin)
	rep := decode[map[string]any](t, raw)
	sum := rep["summary"].(map[string]any)
	if sum["count"].(float64) != 12 || sum["ranges"].(map[string]any)["low"].(float64) <= 0 {
		t.Fatalf("report: %s", raw)
	}
	if code, _ := e.do("GET", "/api/tl/v1/report?tz=Nowhere/Land", nil, admin); code != 400 {
		t.Fatalf("bad tz: %d", code)
	}
}

func TestStreamDeliversNewEntries(t *testing.T) {
	e := newEnv(t, false)
	_, raw := e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "watcher", "role": "reader"}, admin)
	tok := decode[map[string]any](t, raw)["token"].(string)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/api/tl/v1/stream?token="+tok, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	sc := bufio.NewScanner(resp.Body)
	// First event is the current alert list.
	for sc.Scan() && sc.Text() != "" {
	}
	go e.do("POST", "/api/v1/entries", `{"sgv":142,"date":1767225600000}`, admin)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			event = strings.TrimPrefix(line, "event: ")
		}
		if strings.HasPrefix(line, "data: ") {
			data = strings.TrimPrefix(line, "data: ")
		}
		if line == "" && event == "entries" {
			break
		}
	}
	if event != "entries" || !strings.Contains(data, `"sgv":142`) {
		t.Fatalf("got event %q data %q", event, data)
	}
}

func TestImportFromNightscout(t *testing.T) {
	src := newEnv(t, false)
	now := time.Now()
	var batch []map[string]any
	for i := 0; i < 50; i++ {
		batch = append(batch, map[string]any{"sgv": 100 + i, "date": now.Add(-time.Duration(i) * 6 * time.Hour).UnixMilli()})
	}
	src.do("POST", "/api/v1/entries", batch, admin)
	src.do("POST", "/api/v1/treatments", map[string]any{"eventType": "Meal Bolus", "carbs": 40, "insulin": 4,
		"created_at": now.Add(-48 * time.Hour).Format(time.RFC3339)}, admin)
	src.do("POST", "/api/v1/profile", map[string]any{"defaultProfile": "Default"}, admin)

	dst, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	c, err := importer.Run(context.Background(), dst, importer.Options{Source: src.ts.URL, Secret: secret, Days: 10})
	if err != nil {
		t.Fatal(err)
	}
	if c.Entries != 40 || c.Treatments != 1 || c.Profiles != 1 {
		t.Fatalf("counts: %+v", c)
	}
	if _, err := importer.Run(context.Background(), dst, importer.Options{Source: src.ts.URL, Token: "bad-token", Days: 1}); err == nil {
		t.Fatal("expected auth failure with bad token")
	}
}

func TestTokenInUploaderSecretField(t *testing.T) {
	e := newEnv(t, false)
	_, raw := e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "Loop iPhone", "role": "uploader"}, admin)
	tok := decode[map[string]any](t, raw)["token"].(string)
	// Loop/Trio send SHA-1 of whatever was typed into the "API secret" field.
	hdr := map[string]string{"api-secret": sha(tok)}
	if code, raw := e.do("POST", "/api/v1/treatments", `{"eventType":"Note","notes":"hi"}`, hdr); code != 200 {
		t.Fatalf("uploader via sha1 token: %d %s", code, raw)
	}
	_, raw = e.do("GET", "/api/v1/treatments.json", nil, admin)
	if !strings.Contains(string(raw), `"enteredBy":"Loop iPhone"`) {
		t.Fatalf("enteredBy should default to token name: %s", raw)
	}
	if code, _ := e.do("PUT", "/api/tl/v1/settings", settings.Default(), hdr); code != 403 {
		t.Fatalf("uploader must not be admin: %d", code)
	}
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, map[string]string{"api-secret": sha("not-a-token")}); code != 401 {
		t.Fatalf("unknown sha1: %d", code)
	}
}
