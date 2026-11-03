package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// v3do performs a request and also returns the response headers.
func (e *env) v3do(method, path string, body any, hdr map[string]string) (int, map[string]any, http.Header) {
	e.t.Helper()
	var rd *strings.Reader
	switch b := body.(type) {
	case nil:
		rd = strings.NewReader("")
	case string:
		rd = strings.NewReader(b)
	default:
		rd = strings.NewReader(mustJSON(b))
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
	var out map[string]any
	_ = jsonDecode(resp.Body, &out)
	return resp.StatusCode, out, resp.Header
}

// jwtFor creates a token with the role and exchanges it for a JWT, like AndroidAPS does.
func (e *env) jwtFor(role string) (jwt, token string) {
	e.t.Helper()
	_, raw := e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "AAPS " + role, "role": role}, admin)
	token = decode[map[string]any](e.t, raw)["token"].(string)
	code, out, _ := e.v3do("GET", "/api/v2/authorization/request/"+token, nil, nil)
	if code != 200 {
		e.t.Fatalf("auth request: %d %v", code, out)
	}
	if out["exp"].(float64)-out["iat"].(float64) != 8*3600 {
		e.t.Fatalf("jwt lifetime: %v", out)
	}
	return out["token"].(string), token
}

func bearer(jwt string) map[string]string { return map[string]string{"Authorization": "Bearer " + jwt} }

func TestV3VersionAndAuth(t *testing.T) {
	e := newEnv(t, false)
	code, out, _ := e.v3do("GET", "/api/v3/version", nil, nil)
	if code != 200 || out["result"].(map[string]any)["version"] != nsCompatVersion {
		t.Fatalf("version: %d %v", code, out)
	}
	// AndroidAPS first sends an empty bearer and expects 401, then refreshes.
	if code, _, _ := e.v3do("GET", "/api/v3/status", nil, map[string]string{"Authorization": "Bearer "}); code != 401 {
		t.Fatalf("empty bearer: %d", code)
	}
	if code, _, _ := e.v3do("GET", "/api/v2/authorization/request/nope-0123456789abcdef", nil, nil); code != 401 {
		t.Fatalf("bad access token: %d", code)
	}

	jwt, _ := e.jwtFor("uploader")
	code, out, _ = e.v3do("GET", "/api/v3/status", nil, bearer(jwt))
	perms := out["result"].(map[string]any)["apiPermissions"].(map[string]any)
	if code != 200 || perms["treatments"] != "crud" || perms["entries"] != "crud" {
		t.Fatalf("uploader status: %d %v", code, out)
	}
	if code, _, _ := e.v3do("GET", "/api/v2/authorization/debug/check/api:settings:create", nil, bearer(jwt)); code != 200 {
		t.Fatalf("uploader permission check: %d", code)
	}

	rjwt, rtoken := e.jwtFor("reader")
	_, out, _ = e.v3do("GET", "/api/v3/status", nil, bearer(rjwt))
	if out["result"].(map[string]any)["apiPermissions"].(map[string]any)["entries"] != "r" {
		t.Fatalf("reader perms: %v", out)
	}
	if code, _, _ := e.v3do("GET", "/api/v2/authorization/debug/check/api:treatments:create", nil, bearer(rjwt)); code != 403 {
		t.Fatalf("reader create check: %d", code)
	}
	if code, _, _ := e.v3do("POST", "/api/v3/treatments", map[string]any{"eventType": "Note"}, bearer(rjwt)); code != 403 {
		t.Fatalf("reader create: %d", code)
	}

	// Tampered JWTs are rejected.
	bad := jwt[:len(jwt)-2] + "xx"
	if code, _, _ := e.v3do("GET", "/api/v3/status", nil, bearer(bad)); code != 401 {
		t.Fatalf("tampered jwt: %d", code)
	}

	// Revoking the token kills its JWT at once, and a refresh then fails with 401.
	_, raw := e.do("GET", "/api/tl/v1/tokens", nil, admin)
	for _, tk := range decode[[]map[string]any](t, raw) {
		if tk["name"] == "AAPS reader" {
			e.do("DELETE", "/api/tl/v1/tokens/"+tk["id"].(string), nil, admin)
		}
	}
	if code, _, _ := e.v3do("GET", "/api/v3/status", nil, bearer(rjwt)); code != 401 {
		t.Fatalf("revoked jwt: %d", code)
	}
	if code, _, _ := e.v3do("GET", "/api/v2/authorization/request/"+rtoken, nil, nil); code != 401 {
		t.Fatalf("refresh with revoked token: %d", code)
	}
	if code, _, _ := e.v3do("GET", "/api/v3/nope", nil, bearer(jwt)); code != 404 {
		t.Fatalf("unknown collection: %d", code)
	}
}

func TestV3SyncLikeAndroidAPS(t *testing.T) {
	e := newEnv(t, false)
	jwt, _ := e.jwtFor("uploader")
	h := bearer(jwt)
	base := time.Date(2026, 2, 1, 8, 0, 0, 0, time.UTC).UnixMilli()

	// Create readings; a resend is de-duplicated onto the same identifier.
	var ids []string
	for i := 0; i < 3; i++ {
		code, out, _ := e.v3do("POST", "/api/v3/entries", map[string]any{
			"type": "sgv", "sgv": 100 + i, "date": base + int64(i)*300000, "direction": "Flat",
			"device": "AAPS", "app": "AAPS", "utcOffset": 0,
		}, h)
		if code != 201 || out["identifier"] == nil || out["result"].(map[string]any)["identifier"] == nil {
			t.Fatalf("create: %d %v", code, out)
		}
		ids = append(ids, out["identifier"].(string))
	}
	code, out, _ := e.v3do("POST", "/api/v3/entries", map[string]any{"type": "sgv", "sgv": 101, "date": base + 300000}, h)
	if code != 200 || out["isDeduplication"] != true || out["identifier"] != ids[1] {
		t.Fatalf("dedup: %d %v", code, out)
	}

	// Newer-than query, ascending, as getSgvsNewerThan does.
	q := url.Values{"sort": {"date"}, "date$gt": {fmt.Sprint(base)}, "limit": {"10"}}
	_, out, _ = e.v3do("GET", "/api/v3/entries?"+q.Encode(), nil, h)
	res := out["result"].([]any)
	if len(res) != 2 || res[0].(map[string]any)["identifier"] != ids[1] {
		t.Fatalf("newer than: %v", out)
	}
	_, out, _ = e.v3do("GET", "/api/v3/entries?sort$desc=date&type=sgv", nil, h)
	if res := out["result"].([]any); len(res) != 3 || res[0].(map[string]any)["sgv"].(float64) != 102 {
		t.Fatalf("sort desc: %v", out)
	}

	// Full history, then resume from the ETag.
	code, out, hdr := e.v3do("GET", "/api/v3/entries/history/0?limit=100", nil, h)
	etag := hdr.Get("ETag")
	if code != 200 || len(out["result"].([]any)) != 3 || !strings.HasPrefix(etag, `W/"`) {
		t.Fatalf("history: %d %v etag %q", code, out, etag)
	}
	since := etag[3 : len(etag)-1]
	_, out, _ = e.v3do("GET", "/api/v3/entries/history/"+since, nil, h)
	if len(out["result"].([]any)) != 0 {
		t.Fatalf("history after etag should be empty: %v", out)
	}

	// Patch one and delete another: both show up in history, deletion as isValid=false.
	if code, out, _ := e.v3do("PATCH", "/api/v3/entries/"+ids[0], map[string]any{"sgv": 99}, h); code != 200 {
		t.Fatalf("patch: %d %v", code, out)
	}
	if code, _, _ := e.v3do("DELETE", "/api/v3/entries/"+ids[2], nil, h); code != 200 {
		t.Fatal("delete failed")
	}
	_, out, _ = e.v3do("GET", "/api/v3/entries/history/"+since, nil, h)
	hist := out["result"].([]any)
	if len(hist) != 2 {
		t.Fatalf("history after changes: %v", out)
	}
	first, second := hist[0].(map[string]any), hist[1].(map[string]any)
	if first["sgv"].(float64) != 99 || first["modifiedBy"] != "AAPS uploader" || second["isValid"] != false {
		t.Fatalf("history contents: %v", hist)
	}

	if code, _, _ := e.v3do("GET", "/api/v3/entries/"+ids[2], nil, h); code != 410 {
		t.Fatalf("read deleted: %d", code)
	}
	if code, _, _ := e.v3do("DELETE", "/api/v3/entries/"+ids[2], nil, h); code != 404 {
		t.Fatalf("delete twice: %d", code)
	}
	if code, _, _ := e.v3do("PUT", "/api/v3/entries/"+ids[2], map[string]any{"sgv": 1, "date": base}, h); code != 410 {
		t.Fatalf("put deleted: %d", code)
	}
	// Deleted documents disappear from the v1 API too.
	_, raw := e.do("GET", "/api/v1/entries.json?count=10", nil, admin)
	if got := decode[[]map[string]any](t, raw); len(got) != 2 {
		t.Fatalf("v1 after v3 delete: %d", len(got))
	}
	// And re-sending the deleted reading brings it back.
	code, out, _ = e.v3do("POST", "/api/v3/entries", map[string]any{"type": "sgv", "sgv": 102, "date": base + 600000}, h)
	if code != 200 || out["identifier"] != ids[2] {
		t.Fatalf("revive: %d %v", code, out)
	}
}

func TestV3TreatmentsProfileAndMore(t *testing.T) {
	e := newEnv(t, false)
	jwt, _ := e.jwtFor("uploader")
	h := bearer(jwt)

	// AndroidAPS sends date (ms) and may omit created_at.
	date := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	code, out, _ := e.v3do("POST", "/api/v3/treatments", map[string]any{
		"eventType": "Correction Bolus", "insulin": 1.2, "date": date.UnixMilli(), "app": "AAPS", "isSMB": true,
	}, h)
	if code != 201 {
		t.Fatalf("create treatment: %d %v", code, out)
	}
	id := out["identifier"].(string)
	code, out, _ = e.v3do("GET", "/api/v3/treatments/"+id, nil, h)
	doc := out["result"].(map[string]any)
	if code != 200 || doc["created_at"] != "2026-02-01T12:00:00.000Z" || doc["subject"] != "AAPS uploader" {
		t.Fatalf("read treatment: %v", doc)
	}
	q := url.Values{"sort": {"created_at"}, "created_at$gt": {"2026-02-01T11:00:00.000Z"}, "limit": {"10"}}
	_, out, _ = e.v3do("GET", "/api/v3/treatments?"+q.Encode(), nil, h)
	if len(out["result"].([]any)) != 1 {
		t.Fatalf("treatments newer than: %v", out)
	}
	// Treatments written through v1 are visible through v3 and vice versa.
	e.do("POST", "/api/v1/treatments", map[string]any{"eventType": "Note", "notes": "from v1", "created_at": "2026-02-01T13:00:00Z"}, admin)
	_, out, _ = e.v3do("GET", "/api/v3/treatments/history/0", nil, h)
	if len(out["result"].([]any)) != 2 {
		t.Fatalf("mixed history: %v", out)
	}

	// PUT to a new identifier creates.
	code, _, _ = e.v3do("PUT", "/api/v3/treatments/client-chosen-id", map[string]any{"eventType": "Note", "date": date.UnixMilli() + 1000}, h)
	if code != 201 {
		t.Fatalf("put create: %d", code)
	}

	e.v3do("POST", "/api/v3/devicestatus", map[string]any{"device": "openaps://phone", "date": date.UnixMilli(),
		"openaps": map[string]any{"iob": map[string]any{"iob": 0.5}}}, h)
	e.v3do("POST", "/api/v3/profile", map[string]any{"defaultProfile": "Default", "date": date.UnixMilli(),
		"store": map[string]any{"Default": map[string]any{"dia": 5}}}, h)
	e.v3do("POST", "/api/v3/food", map[string]any{"name": "Apple", "carbs": 15, "date": date.UnixMilli()}, h)
	e.v3do("POST", "/api/v3/settings", map[string]any{"key": "aaps", "date": date.UnixMilli()}, h)

	_, out, _ = e.v3do("GET", "/api/v3/profile?sort$desc=date&limit=1", nil, h)
	if res := out["result"].([]any); len(res) != 1 || res[0].(map[string]any)["defaultProfile"] != "Default" {
		t.Fatalf("profile: %v", out)
	}
	_, out, _ = e.v3do("GET", "/api/v3/lastModified", nil, h)
	cols := out["result"].(map[string]any)["collections"].(map[string]any)
	for _, k := range []string{"entries", "treatments", "devicestatus", "profile", "food", "foods", "settings"} {
		if _, ok := cols[k]; !ok {
			t.Fatalf("lastModified missing %s: %v", k, cols)
		}
	}
	if cols["treatments"].(float64) <= 0 || cols["foods"].(float64) <= 0 {
		t.Fatalf("lastModified values: %v", cols)
	}
	// The v1 device status endpoint sees the v3 upload, so the web app shows IOB.
	_, raw := e.do("GET", "/api/v1/devicestatus.json?count=1", nil, admin)
	if !strings.Contains(string(raw), `"openaps"`) {
		t.Fatalf("v1 devicestatus: %s", raw)
	}
}
