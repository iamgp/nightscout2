package dexcom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeShare(t *testing.T, logins *int, expireFirst bool) *httptest.Server {
	reads := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/General/AuthenticatePublisherAccount"):
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["password"] != "pw" || body["applicationId"] != applicationID {
				w.WriteHeader(500)
				w.Write([]byte(`{"Code":"AccountPasswordInvalid","Message":"bad password"}`))
				return
			}
			w.Write([]byte(`"acct-1"`))
		case strings.HasSuffix(r.URL.Path, "/General/LoginPublisherAccountById"):
			*logins++
			w.Write([]byte(`"session-` + string(rune('0'+*logins)) + `"`))
		case strings.HasSuffix(r.URL.Path, "/Publisher/ReadPublisherLatestGlucoseValues"):
			reads++
			if expireFirst && reads == 1 {
				w.WriteHeader(500)
				w.Write([]byte(`{"Code":"SessionIdNotFound","Message":"expired"}`))
				return
			}
			w.Write([]byte(`[
				{"WT":"Date(1767225900000)","ST":"Date(1767225900000)","DT":"Date(1767225900000-0500)","Value":142,"Trend":"FortyFiveUp"},
				{"WT":"Date(1767225600000)","Value":131,"Trend":4},
				{"WT":"garbage","Value":99,"Trend":4}
			]`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
}

func TestLatest(t *testing.T) {
	logins := 0
	srv := fakeShare(t, &logins, false)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Username: "u", Password: "pw"}
	rs, err := c.Latest(context.Background(), 1440, 288)
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].Value != 142 || rs[0].Direction != "FortyFiveUp" || rs[1].Direction != "Flat" {
		t.Fatalf("%+v", rs)
	}
	if !rs[1].Time.Equal(time.UnixMilli(1767225600000)) {
		t.Fatalf("time: %v", rs[1].Time)
	}
	docs := ToEntries(rs)
	if docs[0]["device"] != "share2" || docs[0]["type"] != "sgv" {
		t.Fatalf("%v", docs[0])
	}
}

func TestSessionExpiryRelogs(t *testing.T) {
	logins := 0
	srv := fakeShare(t, &logins, true)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Username: "u", Password: "pw"}
	if _, err := c.Latest(context.Background(), 30, 6); err != nil {
		t.Fatal(err)
	}
	if logins != 2 {
		t.Fatalf("logins: %d", logins)
	}
}

func TestBadPassword(t *testing.T) {
	logins := 0
	srv := fakeShare(t, &logins, false)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Username: "u", Password: "wrong"}
	_, err := c.Latest(context.Background(), 30, 6)
	if err == nil || !strings.Contains(err.Error(), "AccountPasswordInvalid") {
		t.Fatalf("err: %v", err)
	}
}

func TestRegionURL(t *testing.T) {
	if u, _ := RegionURL("OUS"); !strings.Contains(u, "shareous1") {
		t.Fatal(u)
	}
	if _, err := RegionURL("mars"); err == nil {
		t.Fatal("expected error")
	}
}
