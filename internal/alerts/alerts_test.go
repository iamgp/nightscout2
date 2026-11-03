package alerts

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/iamgp/tideline/internal/settings"
	"github.com/iamgp/tideline/internal/store"
)

type fakeData struct{ rs []store.Reading }

func (f *fakeData) SGVs(_ context.Context, from, to time.Time) ([]store.Reading, error) {
	var out []store.Reading
	for _, r := range f.rs {
		if r.Time >= from.UnixMilli() && r.Time < to.UnixMilli() {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeData) LatestSGV(context.Context) (store.Reading, bool, error) {
	if len(f.rs) == 0 {
		return store.Reading{}, false, nil
	}
	return f.rs[len(f.rs)-1], true, nil
}

type sent struct {
	notifier string
	kind     string
	resolved bool
}

type fakeSender struct {
	mu   sync.Mutex
	sent []sent
	wg   sync.WaitGroup
}

func (f *fakeSender) Send(_ context.Context, n settings.Notifier, a Alert, resolved bool) error {
	defer f.wg.Done()
	f.mu.Lock()
	f.sent = append(f.sent, sent{n.ID, a.Kind, resolved})
	f.mu.Unlock()
	return nil
}

type harness struct {
	data   *fakeData
	sender *fakeSender
	eng    *Engine
	now    time.Time
	cfg    settings.Settings
}

func newHarness(notifiers ...settings.Notifier) *harness {
	h := &harness{data: &fakeData{}, sender: &fakeSender{}, now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	h.cfg = settings.Default()
	h.cfg.Notifiers = notifiers
	h.eng = New(h.data, func() settings.Settings { return h.cfg }, nil, h.sender, nil)
	h.eng.now = func() time.Time { return h.now }
	return h
}

// series adds readings every 5 minutes ending at h.now.
func (h *harness) series(vals ...float64) {
	h.data.rs = nil
	for i, v := range vals {
		t := h.now.Add(-time.Duration(len(vals)-1-i) * 5 * time.Minute)
		h.data.rs = append(h.data.rs, store.Reading{Time: t.UnixMilli(), Value: v})
	}
}

// eval runs an evaluation, expecting n notifications, and returns them.
func (h *harness) eval(t *testing.T, n int) []sent {
	t.Helper()
	h.sender.mu.Lock()
	h.sender.sent = nil
	h.sender.mu.Unlock()
	h.sender.wg.Add(n)
	h.eng.Evaluate(context.Background())
	done := make(chan struct{})
	go func() { h.sender.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %d notifications", n)
	}
	time.Sleep(20 * time.Millisecond) // catch unexpected extra sends
	h.sender.mu.Lock()
	defer h.sender.mu.Unlock()
	if len(h.sender.sent) != n {
		t.Fatalf("got %d notifications %+v, want %d", len(h.sender.sent), h.sender.sent, n)
	}
	return append([]sent{}, h.sender.sent...)
}

func kinds(as []Alert) []string {
	var k []string
	for _, a := range as {
		k = append(k, a.Kind)
	}
	return k
}

func phone(id string, escalate int) settings.Notifier {
	return settings.Notifier{ID: id, Name: id, Type: "ntfy", URL: "http://x", MinLevel: "warn", Enabled: true, EscalateAfterMinutes: escalate}
}

func TestNoDataNoAlerts(t *testing.T) {
	h := newHarness(phone("p", 0))
	h.eval(t, 0)
	if len(h.eng.Active()) != 0 {
		t.Fatal("expected no alerts")
	}
}

func TestGlucoseLevels(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{{50, "urgent_low"}, {55, "urgent_low"}, {65, "low"}, {120, ""}, {200, "high"}, {260, "urgent_high"}}
	for _, c := range cases {
		h := newHarness()
		h.series(120, 120, c.v)
		h.eng.Evaluate(context.Background())
		got := kinds(h.eng.Active())
		if c.want == "" && len(got) != 0 || c.want != "" && (len(got) != 1 || got[0] != c.want) {
			t.Errorf("value %v: got %v, want %q", c.v, got, c.want)
		}
	}
}

func TestDisabledAlertKind(t *testing.T) {
	h := newHarness()
	h.cfg.Alerts.Enabled[settings.AlertHigh] = false
	h.series(200)
	h.eng.Evaluate(context.Background())
	if len(h.eng.Active()) != 0 {
		t.Fatalf("disabled high alert fired: %v", kinds(h.eng.Active()))
	}
}

func TestPredictedLow(t *testing.T) {
	h := newHarness()
	h.series(130, 118, 106, 94, 84) // falling ~2.3 mg/dL/min
	h.eng.Evaluate(context.Background())
	if got := kinds(h.eng.Active()); len(got) != 1 || got[0] != "predicted_low" {
		t.Fatalf("got %v", got)
	}
	h.series(100, 101, 99, 100)
	h.eng.Evaluate(context.Background())
	if got := kinds(h.eng.Active()); len(got) != 0 {
		t.Fatalf("stable glucose: got %v", got)
	}
}

func TestStaleData(t *testing.T) {
	h := newHarness()
	h.series(250) // would be urgent high if fresh
	h.now = h.now.Add(20 * time.Minute)
	h.eng.Evaluate(context.Background())
	if got := kinds(h.eng.Active()); len(got) != 1 || got[0] != "stale" {
		t.Fatalf("20 min: got %v", got)
	}
	h.now = h.now.Add(15 * time.Minute)
	h.eng.Evaluate(context.Background())
	if got := kinds(h.eng.Active()); len(got) != 1 || got[0] != "urgent_stale" {
		t.Fatalf("35 min: got %v", got)
	}
}

func TestNotifyRepeatEscalateResolve(t *testing.T) {
	h := newHarness(phone("phone", 0), phone("partner", 10))
	h.series(110, 90, 50)

	got := h.eval(t, 1)
	if got[0].notifier != "phone" || got[0].kind != "urgent_low" {
		t.Fatalf("first: %+v", got)
	}
	h.now = h.now.Add(2 * time.Minute)
	h.eval(t, 0) // within the 5-minute urgent repeat interval

	h.now = h.now.Add(4 * time.Minute) // 6 min: repeat to phone, partner not yet
	got = h.eval(t, 1)
	if got[0].notifier != "phone" {
		t.Fatalf("repeat: %+v", got)
	}

	h.now = h.now.Add(5 * time.Minute) // 11 min: escalate to partner, repeat phone
	got = h.eval(t, 2)
	seen := map[string]bool{}
	for _, s := range got {
		seen[s.notifier] = true
	}
	if !seen["phone"] || !seen["partner"] {
		t.Fatalf("escalation: %+v", got)
	}

	h.series(90, 100, 110) // back in range: both notified it's resolved
	got = h.eval(t, 2)
	for _, s := range got {
		if !s.resolved {
			t.Fatalf("expected resolved notifications: %+v", got)
		}
	}
}

func TestSnoozeSuppressesNotifications(t *testing.T) {
	h := newHarness(phone("phone", 0))
	h.series(65)
	h.eval(t, 1)
	if err := h.eng.Snooze("low", 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(20 * time.Minute)
	h.series(65)
	h.eval(t, 0)
	if a := h.eng.Active(); len(a) != 1 || a[0].SnoozedUntil == nil {
		t.Fatalf("snoozed alert should stay visible: %+v", a)
	}
	h.now = h.now.Add(15 * time.Minute) // snooze expired
	h.series(65)
	h.eval(t, 1)
	if err := h.eng.Snooze("bogus", time.Minute); err == nil {
		t.Fatal("expected error for unknown kind")
	}
}

func TestEscalationWithinFamilyDoesNotSendResolved(t *testing.T) {
	h := newHarness(phone("phone", 0))
	h.series(65)
	h.eval(t, 1) // low
	h.series(65, 50)
	got := h.eval(t, 1) // urgent low, no "low resolved"
	if got[0].kind != "urgent_low" || got[0].resolved {
		t.Fatalf("got %+v", got)
	}
}

func TestUrgentOnlyNotifier(t *testing.T) {
	n := phone("pager", 0)
	n.MinLevel = "urgent"
	h := newHarness(n)
	h.series(200)
	h.eval(t, 0)
	h.series(260)
	h.eval(t, 1)
}

func TestHTTPSenderNtfy(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b := make([]byte, 256)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
	}))
	defer srv.Close()
	s := &HTTPSender{Units: func() string { return settings.UnitsMmol }}
	err := s.Send(context.Background(), settings.Notifier{Type: "ntfy", URL: srv.URL},
		Alert{Kind: "urgent_low", Level: LevelUrgent, Title: "Urgent low", Value: 50}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Header.Get("Priority") != "5" || got.Header.Get("Title") != "Tideline: Urgent low" || body != "2.8 mmol/L" {
		t.Fatalf("headers %v body %q", got.Header, body)
	}
}

func TestProject(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rs := []store.Reading{
		{Time: base.UnixMilli(), Value: 100},
		{Time: base.Add(5 * time.Minute).UnixMilli(), Value: 90},
		{Time: base.Add(10 * time.Minute).UnixMilli(), Value: 80},
	}
	v, ok := Project(rs, base.Add(30*time.Minute))
	if !ok || v < 39.9 || v > 40.1 {
		t.Fatalf("got %v %v, want 40", v, ok)
	}
	if _, ok := Project(rs[:2], base); ok {
		t.Fatal("two readings should not project")
	}
}
