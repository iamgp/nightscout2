package server

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/iamgp/tideline/internal/alerts"
	"github.com/iamgp/tideline/internal/webpush"
)

// fakePushService plays a browser's push service: it decrypts what it
// receives with the subscriber's keys.
type fakePushService struct {
	srv  *httptest.Server
	priv *ecdh.PrivateKey
	auth []byte

	mu       sync.Mutex
	received []map[string]any
	notify   chan struct{}
}

func newFakePush(t *testing.T) *fakePushService {
	f := &fakePushService{notify: make(chan struct{}, 16)}
	f.priv, _ = ecdh.P256().GenerateKey(rand.Reader)
	f.auth = make([]byte, 16)
	rand.Read(f.auth)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		plain, err := webpush.Decrypt(f.priv, f.auth, body)
		if err != nil {
			t.Errorf("push decrypt: %v", err)
			w.WriteHeader(400)
			return
		}
		var m map[string]any
		json.Unmarshal(plain, &m)
		f.mu.Lock()
		f.received = append(f.received, m)
		f.mu.Unlock()
		f.notify <- struct{}{}
		w.WriteHeader(201)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakePushService) subscription(path string) map[string]any {
	b64 := base64.RawURLEncoding
	return map[string]any{
		"endpoint": f.srv.URL + path,
		"keys":     map[string]string{"p256dh": b64.EncodeToString(f.priv.PublicKey().Bytes()), "auth": b64.EncodeToString(f.auth)},
	}
}

func (f *fakePushService) wait(t *testing.T) map[string]any {
	t.Helper()
	select {
	case <-f.notify:
	case <-time.After(3 * time.Second):
		t.Fatal("no push received")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.received[len(f.received)-1]
}

func (f *fakePushService) quiet(t *testing.T) {
	t.Helper()
	select {
	case <-f.notify:
		t.Fatal("unexpected push")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestWebPushEndToEnd(t *testing.T) {
	keys, _ := webpush.GenerateKeys()
	pc := &webpush.Client{Keys: keys, Subject: "https://example.org"}
	push := newFakePush(t)
	e := newEnv(t, false, func(o *Options) {
		o.Push = pc
		o.Sender = &alerts.HTTPSender{Push: pc}
		o.PushAllowHTTP = true
		o.PushHostAllowed = func(h string) bool { return h == "127.0.0.1" }
	})

	_, raw := e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "Grandma", "role": "reader"}, admin)
	gran := map[string]string{"Authorization": "Bearer " + decode[map[string]any](t, raw)["token"].(string)}
	_, raw = e.do("POST", "/api/tl/v1/tokens", map[string]any{"name": "Neighbour", "role": "reader"}, admin)
	other := map[string]string{"Authorization": "Bearer " + decode[map[string]any](t, raw)["token"].(string)}

	code, raw := e.do("GET", "/api/tl/v1/push/key", nil, gran)
	if code != 200 || decode[map[string]any](t, raw)["publicKey"] != keys.PublicKey() {
		t.Fatalf("key: %d %s", code, raw)
	}

	// A follower registers their phone.
	code, raw = e.do("POST", "/api/tl/v1/push/subscriptions", map[string]any{
		"subscription": push.subscription("/gran-phone"), "name": "Grandma's iPhone", "minLevel": "warn",
	}, gran)
	if code != 201 {
		t.Fatalf("subscribe: %d %s", code, raw)
	}
	subID := decode[map[string]any](t, raw)["id"].(string)

	// Test notification reaches the device.
	if code, raw := e.do("POST", "/api/tl/v1/push/subscriptions/"+subID+"/test", nil, gran); code != 200 {
		t.Fatalf("test push: %d %s", code, raw)
	}
	if m := push.wait(t); m["title"] != "Tideline: Test notification" {
		t.Fatalf("test payload: %v", m)
	}

	// Someone else can't see or remove it; the admin can see it.
	_, raw = e.do("GET", "/api/tl/v1/push/subscriptions", nil, other)
	if n := len(decode[[]any](t, raw)); n != 0 {
		t.Fatalf("other reader sees %d subscriptions", n)
	}
	if code, _ := e.do("DELETE", "/api/tl/v1/push/subscriptions/"+subID, nil, other); code != 404 {
		t.Fatalf("other reader delete: %d", code)
	}
	_, raw = e.do("GET", "/api/tl/v1/push/subscriptions", nil, admin)
	if subs := decode[[]map[string]any](t, raw); len(subs) != 1 || subs[0]["owner"] != "Grandma" || subs[0]["endpoint"] != nil {
		t.Fatalf("admin list (endpoint must not leak): %v", subs)
	}

	// A low reading pushes an alert to the device.
	e.do("POST", "/api/v1/entries", map[string]any{"sgv": 62, "date": time.Now().UnixMilli()}, admin)
	e.srv.Alerts().Evaluate(context.Background())
	m := push.wait(t)
	if m["title"] != "Tideline: Low" || m["level"] != "warn" || m["tag"] != "tideline-low" || m["resolved"] != false {
		t.Fatalf("alert payload: %v", m)
	}

	// Revoking the follower's token stops alerts to their devices.
	_, raw = e.do("GET", "/api/tl/v1/tokens", nil, admin)
	for _, tk := range decode[[]map[string]any](t, raw) {
		if tk["name"] == "Grandma" {
			e.do("DELETE", "/api/tl/v1/tokens/"+tk["id"].(string), nil, admin)
		}
	}
	e.do("POST", "/api/v1/entries", map[string]any{"sgv": 50, "date": time.Now().UnixMilli() + 1000}, admin)
	e.srv.Alerts().Evaluate(context.Background())
	push.quiet(t)
}

func TestPushRejectsUnknownEndpoints(t *testing.T) {
	keys, _ := webpush.GenerateKeys()
	e := newEnv(t, false, func(o *Options) { o.Push = &webpush.Client{Keys: keys} })
	push := newFakePush(t)
	for _, ep := range []string{
		"http://169.254.169.254/latest/meta-data",
		"https://internal.example.com/hook",
		"https://fcm.googleapis.com.evil.example/x",
		push.srv.URL + "/x", // plain HTTP on localhost
	} {
		sub := push.subscription("")
		sub["endpoint"] = ep
		if code, raw := e.do("POST", "/api/tl/v1/push/subscriptions", map[string]any{"subscription": sub}, admin); code != 400 {
			t.Errorf("%s: got %d %s, want 400", ep, code, raw)
		}
	}
	if !DefaultPushHostAllowed("fcm.googleapis.com") || !DefaultPushHostAllowed("web.push.apple.com") ||
		!DefaultPushHostAllowed("updates.push.services.mozilla.com") || DefaultPushHostAllowed("googleapis.com.evil.io") {
		t.Fatal("allowlist")
	}
}
