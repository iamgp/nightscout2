package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestAuthLimiterLeakyBucket(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newAuthLimiter(10, time.Minute)
	l.now = func() time.Time { return now }

	for i := 0; i < 9; i++ {
		if l.Fail("1.2.3.4") {
			t.Fatalf("blocked early at %d", i)
		}
	}
	if l.Blocked("1.2.3.4") != 0 {
		t.Fatal("9 failures should not block")
	}
	if !l.Fail("1.2.3.4") {
		t.Fatal("10th failure should block")
	}
	if l.Fail("1.2.3.4") {
		t.Fatal("justBlocked must only fire once")
	}
	if w := l.Blocked("1.2.3.4"); w < time.Minute || w > 3*time.Minute {
		t.Fatalf("wait %v", w)
	}
	if l.Blocked("5.6.7.8") != 0 {
		t.Fatal("other IPs unaffected")
	}
	now = now.Add(2 * time.Minute)
	if l.Blocked("1.2.3.4") != 0 {
		t.Fatal("should drain after two minutes")
	}

	// A device retrying a stale token every 5 minutes never gets blocked.
	l2 := newAuthLimiter(10, time.Minute)
	clock := now
	l2.now = func() time.Time { return clock }
	for i := 0; i < 500; i++ {
		l2.Fail("9.9.9.9")
		clock = clock.Add(5 * time.Minute)
	}
	if l2.Blocked("9.9.9.9") != 0 {
		t.Fatal("slow retries must not block")
	}
}

func TestClientIP(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("Fly-Client-IP", "203.0.113.9")
	if got := s.clientIP(r); got != "10.0.0.1" {
		t.Fatalf("headers must be ignored unless configured: %s", got)
	}
	s.opts.ClientIPHeader = "Fly-Client-IP"
	if got := s.clientIP(r); got != "203.0.113.9" {
		t.Fatalf("fly: %s", got)
	}
	s.opts.ClientIPHeader = "X-Forwarded-For"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.7")
	if got := s.clientIP(r); got != "198.51.100.7" {
		t.Fatalf("xff must use the proxy-appended (rightmost) entry: %s", got)
	}
	r.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := s.clientIP(r); got != "10.0.0.1" {
		t.Fatalf("garbage header falls back to peer: %s", got)
	}
}

func TestSignInRateLimiting(t *testing.T) {
	e := newEnv(t, false, func(o *Options) {
		o.AuthFailureBurst = 3
		o.ClientIPHeader = "Fly-Client-IP"
	})
	from := func(ip string, h map[string]string) map[string]string {
		out := map[string]string{"Fly-Client-IP": ip}
		for k, v := range h {
			out[k] = v
		}
		return out
	}
	// A follower's device that has already signed in successfully.
	_, raw := e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "Mum", "role": "reader"}, admin)
	mum := map[string]string{"Authorization": "Bearer " + decode[map[string]any](t, raw)["token"].(string)}
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, from("203.0.113.5", mum)); code != 200 {
		t.Fatal("mum's first request")
	}
	// A valid token that has never been used yet.
	_, raw = e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "New phone", "role": "reader"}, admin)
	fresh := map[string]string{"Authorization": "Bearer " + decode[map[string]any](t, raw)["token"].(string)}

	// An attacker on the same address guesses.
	for i := 0; i < 3; i++ {
		if code, _ := e.do("GET", "/api/v1/entries.json", nil, from("203.0.113.5", map[string]string{"api-secret": sha("guess" + strconv.Itoa(i))})); code != 401 {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/entries.json", nil)
	req.Header.Set("Fly-Client-IP", "203.0.113.5")
	req.Header.Set("api-secret", sha("guess-4"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("blocked guess: %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	// Even a valid credential is refused from the blocked address if it has
	// never been used, so guessing can't succeed during the block.
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, from("203.0.113.5", fresh)); code != 429 {
		t.Fatalf("unseen valid token while blocked: %d", code)
	}
	// Mum's device keeps working.
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, from("203.0.113.5", mum)); code != 200 {
		t.Fatalf("known credential while blocked: %d", code)
	}
	// The JWT exchange and verifyauth are covered too.
	if code, _ := e.do("GET", "/api/v2/authorization/request/guess-0123456789abcdef", nil, from("203.0.113.5", nil)); code != 429 {
		t.Fatalf("jwt exchange while blocked: %d", code)
	}
	if code, _ := e.do("GET", "/api/v1/verifyauth", nil, from("203.0.113.5", map[string]string{"api-secret": "x"})); code != 429 {
		t.Fatalf("verifyauth while blocked: %d", code)
	}
	// Other addresses are unaffected.
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, from("198.51.100.1", admin)); code != 200 {
		t.Fatalf("other ip: %d", code)
	}
	// The block is in the audit log.
	_, raw = e.do("GET", "/api/tl/v1/audit", nil, from("198.51.100.1", admin))
	if got := string(raw); !contains(got, "sign-in blocked") || !contains(got, "203.0.113.5") {
		t.Fatalf("audit: %s", got)
	}
}

func TestVerifyAuthFailuresCount(t *testing.T) {
	e := newEnv(t, false, func(o *Options) { o.AuthFailureBurst = 2 })
	for i := 0; i < 2; i++ {
		e.do("GET", "/api/v1/verifyauth", nil, map[string]string{"api-secret": "nope"})
	}
	if code, _ := e.do("GET", "/api/v1/status.json", nil, nil); code != 200 {
		t.Fatalf("status stays public: %d", code)
	}
	if code, _ := e.do("GET", "/api/v1/entries.json", nil, map[string]string{"api-secret": "nope"}); code != 429 {
		t.Fatalf("after verifyauth failures: %d", code)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
