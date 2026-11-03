// Package alerts evaluates glucose data on the server and raises alerts.
//
// Evaluating on the server (rather than in a browser tab, as Nightscout does)
// means alerts fire even when nobody has the site open. Alerts are pushed to
// connected clients over the event stream and to configured notifiers, with
// per-notifier escalation delays and repeat intervals.
package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/iamgp/tideline/internal/settings"
	"github.com/iamgp/tideline/internal/store"
)

// Levels.
const (
	LevelWarn   = "warn"
	LevelUrgent = "urgent"
)

// Alert is an active alert condition.
type Alert struct {
	Kind         string     `json:"kind"`
	Level        string     `json:"level"`
	Title        string     `json:"title"`
	Message      string     `json:"message"`
	Value        float64    `json:"value,omitempty"` // mg/dL, for glucose alerts
	Since        time.Time  `json:"since"`
	SnoozedUntil *time.Time `json:"snoozedUntil,omitempty"`

	notified map[string]time.Time // notifier ID -> last sent
}

// DataSource provides recent glucose readings.
type DataSource interface {
	SGVs(ctx context.Context, from, to time.Time) ([]store.Reading, error)
	LatestSGV(ctx context.Context) (store.Reading, bool, error)
}

// Sender delivers a notification. resolved is true when the alert has cleared.
type Sender interface {
	Send(ctx context.Context, n settings.Notifier, a Alert, resolved bool) error
}

// Engine evaluates alert conditions.
type Engine struct {
	data     DataSource
	settings func() settings.Settings
	publish  func([]Alert)
	sender   Sender
	log      *slog.Logger
	now      func() time.Time

	mu      sync.Mutex
	active  map[string]*Alert
	snoozes map[string]time.Time
}

// New creates an alert engine. publish is called with the full active list
// whenever it changes.
func New(data DataSource, s func() settings.Settings, publish func([]Alert), sender Sender, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	return &Engine{
		data: data, settings: s, publish: publish, sender: sender, log: log,
		now:     time.Now,
		active:  map[string]*Alert{},
		snoozes: map[string]time.Time{},
	}
}

// Run evaluates on an interval until ctx is cancelled.
func (e *Engine) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	e.Evaluate(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Evaluate(ctx)
		}
	}
}

// Active returns the current alerts, most severe first.
func (e *Engine) Active() []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.listLocked()
}

func (e *Engine) listLocked() []Alert {
	out := make([]Alert, 0, len(e.active))
	for _, a := range e.active {
		c := *a
		c.notified = nil
		if until, ok := e.snoozes[a.Kind]; ok && until.After(e.now()) {
			u := until
			c.SnoozedUntil = &u
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level == LevelUrgent
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// Snooze silences an alert kind for d. A zero duration clears the snooze.
func (e *Engine) Snooze(kind string, d time.Duration) error {
	known := false
	for _, k := range settings.AllAlertKinds {
		if k == kind {
			known = true
		}
	}
	if !known {
		return fmt.Errorf("unknown alert kind %q", kind)
	}
	e.mu.Lock()
	if d <= 0 {
		delete(e.snoozes, kind)
	} else {
		e.snoozes[kind] = e.now().Add(d)
	}
	list := e.listLocked()
	e.mu.Unlock()
	if e.publish != nil {
		e.publish(list)
	}
	return nil
}

// condition is a desired alert computed from the data.
type condition struct {
	kind, level, title, message string
	value                       float64
}

func (e *Engine) conditions(ctx context.Context, s settings.Settings) ([]condition, error) {
	now := e.now()
	latest, ok, err := e.data.LatestSGV(ctx)
	if err != nil || !ok {
		return nil, err
	}
	var out []condition
	en := s.Alerts.Enabled
	age := now.Sub(time.UnixMilli(latest.Time))
	mins := int(age.Minutes())
	switch {
	case age >= time.Duration(s.Alerts.UrgentStaleMinutes)*time.Minute && en[settings.AlertUrgentStale]:
		out = append(out, condition{settings.AlertUrgentStale, LevelUrgent, "No CGM data",
			fmt.Sprintf("Last reading was %d minutes ago", mins), 0})
	case age >= time.Duration(s.Alerts.StaleMinutes)*time.Minute && en[settings.AlertStale]:
		out = append(out, condition{settings.AlertStale, LevelWarn, "CGM data is stale",
			fmt.Sprintf("Last reading was %d minutes ago", mins), 0})
	}
	// Don't raise glucose alerts from an old reading.
	if age >= time.Duration(s.Alerts.StaleMinutes)*time.Minute {
		return out, nil
	}
	v, t := latest.Value, s.Thresholds
	switch {
	case v <= t.UrgentLow:
		if en[settings.AlertUrgentLow] {
			out = append(out, condition{settings.AlertUrgentLow, LevelUrgent, "Urgent low", "", v})
		}
	case v < t.Low:
		if en[settings.AlertLow] {
			out = append(out, condition{settings.AlertLow, LevelWarn, "Low", "", v})
		}
	case v >= t.UrgentHigh:
		if en[settings.AlertUrgentHigh] {
			out = append(out, condition{settings.AlertUrgentHigh, LevelUrgent, "Urgent high", "", v})
		}
	case v > t.High:
		if en[settings.AlertHigh] {
			out = append(out, condition{settings.AlertHigh, LevelWarn, "High", "", v})
		}
	}
	if v >= t.Low && en[settings.AlertPredictedLow] {
		horizon := time.Duration(s.Alerts.PredictMinutes) * time.Minute
		recent, err := e.data.SGVs(ctx, now.Add(-25*time.Minute), now.Add(time.Minute))
		if err != nil {
			return nil, err
		}
		if p, ok := Project(recent, time.UnixMilli(latest.Time).Add(horizon)); ok && p < t.Low {
			out = append(out, condition{settings.AlertPredictedLow, LevelWarn, "Low predicted",
				fmt.Sprintf("Projected %.0f mg/dL in %d minutes", p, s.Alerts.PredictMinutes), v})
		}
	}
	return out, nil
}

// Project fits a least-squares line through readings and extrapolates to at.
// It needs at least three readings spanning ten minutes or more.
func Project(rs []store.Reading, at time.Time) (float64, bool) {
	if len(rs) < 3 {
		return 0, false
	}
	t0 := rs[0].Time
	if rs[len(rs)-1].Time-t0 < 10*60*1000 {
		return 0, false
	}
	var sx, sy, sxx, sxy float64
	n := float64(len(rs))
	for _, r := range rs {
		x := float64(r.Time-t0) / 60000
		sx += x
		sy += r.Value
		sxx += x * x
		sxy += x * r.Value
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0, false
	}
	slope := (n*sxy - sx*sy) / den
	icept := (sy - slope*sx) / n
	x := float64(at.UnixMilli()-t0) / 60000
	return icept + slope*x, true
}

// Evaluate recomputes alerts and sends any due notifications.
func (e *Engine) Evaluate(ctx context.Context) {
	s := e.settings()
	conds, err := e.conditions(ctx, s)
	if err != nil {
		e.log.Error("alert evaluation failed", "err", err)
		return
	}
	now := e.now()

	type job struct {
		n        settings.Notifier
		a        Alert
		resolved bool
	}
	var jobs []job

	e.mu.Lock()
	changed := false
	want := map[string]condition{}
	for _, c := range conds {
		want[c.kind] = c
	}
	for kind, a := range e.active {
		if _, ok := want[kind]; !ok {
			// Only announce "resolved" when the whole family has cleared, so
			// low -> urgent low doesn't send a misleading "low resolved".
			familyActive := false
			for k := range want {
				if family(k) == family(kind) {
					familyActive = true
				}
			}
			for _, n := range s.Notifiers {
				if familyActive {
					break
				}
				if _, sent := a.notified[n.ID]; sent && n.Enabled {
					jobs = append(jobs, job{n, *a, true})
				}
			}
			delete(e.active, kind)
			changed = true
		}
	}
	for kind, c := range want {
		a, ok := e.active[kind]
		if !ok {
			a = &Alert{Kind: kind, Since: now, notified: map[string]time.Time{}}
			e.active[kind] = a
			changed = true
		}
		if a.Value != c.value || a.Message != c.message {
			changed = true
		}
		a.Level, a.Title, a.Message, a.Value = c.level, c.title, c.message, c.value
	}
	for kind, until := range e.snoozes {
		if until.Before(now) {
			delete(e.snoozes, kind)
			changed = true
		}
	}
	for _, a := range e.active {
		if until, ok := e.snoozes[a.Kind]; ok && until.After(now) {
			continue
		}
		repeat := time.Duration(s.Alerts.RepeatWarnMinutes) * time.Minute
		if a.Level == LevelUrgent {
			repeat = time.Duration(s.Alerts.RepeatUrgentMinutes) * time.Minute
		}
		for _, n := range s.Notifiers {
			if !n.Enabled || (n.MinLevel == LevelUrgent && a.Level != LevelUrgent) {
				continue
			}
			if now.Sub(a.Since) < time.Duration(n.EscalateAfterMinutes)*time.Minute {
				continue
			}
			if last, sent := a.notified[n.ID]; sent && now.Sub(last) < repeat {
				continue
			}
			a.notified[n.ID] = now
			jobs = append(jobs, job{n, *a, false})
		}
	}
	list := e.listLocked()
	e.mu.Unlock()

	if changed && e.publish != nil {
		e.publish(list)
	}
	for _, j := range jobs {
		if e.sender == nil {
			continue
		}
		go func(j job) {
			sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if err := e.sender.Send(sctx, j.n, j.a, j.resolved); err != nil {
				e.log.Warn("notification failed", "notifier", j.n.Name, "kind", j.a.Kind, "err", err)
			}
		}(j)
	}
}

func family(kind string) string {
	switch kind {
	case settings.AlertUrgentLow, settings.AlertLow, settings.AlertPredictedLow:
		return "low"
	case settings.AlertHigh, settings.AlertUrgentHigh:
		return "high"
	}
	return "stale"
}
