package libre

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeLibre emulates LibreLinkUp on two "hosts" (global and eu), told apart
// by path prefix since httptest only gives us one address.
type fakeLibre struct {
	t           *testing.T
	srv         *httptest.Server
	logins      int
	expireNext  bool
	termsNeeded bool
}

func newFake(t *testing.T) *fakeLibre {
	f := &fakeLibre{t: t}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLibre) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("product") != "llu.ios" || r.Header.Get("version") == "" {
		w.WriteHeader(403)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	global := strings.HasPrefix(r.URL.Path, "/global")
	path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/global"), "/eu")
	switch path {
	case "/llu/auth/login":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["password"] != "pw" {
			w.Write([]byte(`{"status":2,"error":{"message":"notAuthenticated"}}`))
			return
		}
		if global {
			w.Write([]byte(`{"status":0,"data":{"redirect":true,"region":"eu"}}`))
			return
		}
		if f.termsNeeded {
			w.Write([]byte(`{"status":4,"data":{"step":{"type":"tou","componentName":"AcceptDocument"}}}`))
			return
		}
		f.logins++
		exp := time.Now().Add(time.Hour).Unix()
		json.NewEncoder(w).Encode(map[string]any{"status": 0, "data": map[string]any{
			"user":       map[string]any{"id": "user-123"},
			"authTicket": map[string]any{"token": "tok", "expires": exp, "duration": 3600000},
		}})
	case "/llu/connections", "/llu/connections/patient-1/graph":
		sum := sha256.Sum256([]byte("user-123"))
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("account-id") != hex.EncodeToString(sum[:]) {
			w.WriteHeader(401)
			return
		}
		if f.expireNext {
			f.expireNext = false
			w.WriteHeader(401)
			return
		}
		if path == "/llu/connections" {
			w.Write([]byte(`{"status":0,"data":[{"patientId":"patient-1","firstName":"Sam"}]}`))
			return
		}
		w.Write([]byte(`{"status":0,"data":{
			"connection":{"patientId":"patient-1","glucoseMeasurement":{"FactoryTimestamp":"11/3/2026 7:35:00 PM","Timestamp":"11/3/2026 2:35:00 PM","ValueInMgPerDl":142,"TrendArrow":4}},
			"graphData":[
				{"FactoryTimestamp":"11/3/2026 7:20:00 PM","ValueInMgPerDl":130},
				{"FactoryTimestamp":"11/3/2026 7:35:00 PM","ValueInMgPerDl":142},
				{"FactoryTimestamp":"garbage","ValueInMgPerDl":99}
			]}}`))
	default:
		f.t.Errorf("unexpected path %s", r.URL.Path)
		w.WriteHeader(404)
	}
}

// client returns a client whose "hosts" map onto the fake server's paths.
func (f *fakeLibre) client(password string) *Client {
	addr := strings.TrimPrefix(f.srv.URL, "http://")
	return &Client{Host: addr + "/global", Email: "a@b.c", Password: password, Scheme: "http",
		HTTP: &http.Client{Transport: regionRewrite{addr}}}
}

// regionRewrite sends requests for real regional hosts to the fake's /eu prefix.
type regionRewrite struct{ addr string }

func (rr regionRewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == regions["eu"] {
		r.URL.Host = rr.addr
		r.URL.Path = "/eu" + r.URL.Path
		r.URL.Scheme = "http"
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestLatestFollowsRegionRedirect(t *testing.T) {
	f := newFake(t)
	c := f.client("pw")
	rs, err := c.Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != regions["eu"] {
		t.Fatalf("host after redirect: %s", c.Host)
	}
	if len(rs) != 2 {
		t.Fatalf("readings: %+v", rs)
	}
	cur := rs[0]
	if cur.Value != 142 || cur.Direction != "FortyFiveUp" || !cur.Time.Equal(time.Date(2026, 11, 3, 19, 35, 0, 0, time.UTC)) {
		t.Fatalf("current: %+v", cur)
	}
	if rs[1].Value != 130 || rs[1].Direction != "" {
		t.Fatalf("history: %+v", rs[1])
	}
	docs := ToEntries(rs)
	if docs[0]["device"] != "librelinkup" || docs[0]["direction"] != "FortyFiveUp" || docs[1]["direction"] != nil {
		t.Fatalf("entries: %v", docs)
	}
}

func TestReloginWhenSessionExpires(t *testing.T) {
	f := newFake(t)
	c := f.client("pw")
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.expireNext = true
	if _, err := c.Latest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.logins != 2 {
		t.Fatalf("logins: %d", f.logins)
	}
}

func TestActionableErrors(t *testing.T) {
	f := newFake(t)
	_, err := f.client("wrong").Latest(context.Background())
	var le *Error
	if !errors.As(err, &le) || !strings.Contains(le.Msg, "LIBRE_PASSWORD") {
		t.Fatalf("bad password: %v", err)
	}
	f.termsNeeded = true
	_, err = f.client("pw").Latest(context.Background())
	if !errors.As(err, &le) || !strings.Contains(le.Msg, "accept") {
		t.Fatalf("terms: %v", err)
	}
	c := f.client("pw")
	f.termsNeeded = false
	c.PatientID = "someone-else"
	if _, err = c.Latest(context.Background()); !errors.As(err, &le) || !strings.Contains(le.Msg, "LIBRE_PATIENT_ID") {
		t.Fatalf("patient id: %v", err)
	}
}

func TestRegionHost(t *testing.T) {
	if h, _ := RegionHost(""); h != GlobalHost {
		t.Fatal(h)
	}
	if h, _ := RegionHost("US"); h != "api-us.libreview.io" {
		t.Fatal(h)
	}
	if _, err := RegionHost("mars"); err == nil {
		t.Fatal("expected error")
	}
}
