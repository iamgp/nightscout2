// Package settings holds Tideline's user-editable configuration: display
// units, glucose thresholds, alert rules and notification targets.
//
// All glucose values are stored in mg/dL internally; conversion to mmol/L is
// a display concern only.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// Alert kinds understood by the alert engine.
const (
	AlertUrgentLow    = "urgent_low"
	AlertLow          = "low"
	AlertPredictedLow = "predicted_low"
	AlertHigh         = "high"
	AlertUrgentHigh   = "urgent_high"
	AlertStale        = "stale"
	AlertUrgentStale  = "urgent_stale"
)

// AllAlertKinds lists every alert kind, most severe first within each family.
var AllAlertKinds = []string{
	AlertUrgentLow, AlertLow, AlertPredictedLow, AlertHigh, AlertUrgentHigh, AlertStale, AlertUrgentStale,
}

// Units for display.
const (
	UnitsMgdl = "mg/dl"
	UnitsMmol = "mmol"
)

// Thresholds in mg/dL. The target range is [Low, High].
type Thresholds struct {
	UrgentLow  float64 `json:"urgentLow"`
	Low        float64 `json:"low"`
	High       float64 `json:"high"`
	UrgentHigh float64 `json:"urgentHigh"`
}

// AlertSettings controls when the server raises alerts.
type AlertSettings struct {
	Enabled             map[string]bool `json:"enabled"`
	StaleMinutes        int             `json:"staleMinutes"`
	UrgentStaleMinutes  int             `json:"urgentStaleMinutes"`
	PredictMinutes      int             `json:"predictMinutes"`
	RepeatWarnMinutes   int             `json:"repeatWarnMinutes"`
	RepeatUrgentMinutes int             `json:"repeatUrgentMinutes"`
}

// Notifier is a destination for alert notifications.
//
// Notifiers form an escalation chain: a notifier with EscalateAfterMinutes > 0
// is only contacted once an alert has stayed active and un-snoozed for that
// long. That lets you page the phone first and a partner later.
type Notifier struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Type                 string `json:"type"` // ntfy | webhook | pushover
	URL                  string `json:"url,omitempty"`
	Token                string `json:"token,omitempty"`
	User                 string `json:"user,omitempty"`
	MinLevel             string `json:"minLevel"` // warn | urgent
	EscalateAfterMinutes int    `json:"escalateAfterMinutes"`
	Enabled              bool   `json:"enabled"`
}

// Settings is the full settings document.
type Settings struct {
	Name       string        `json:"name"`
	Units      string        `json:"units"`
	Thresholds Thresholds    `json:"thresholds"`
	Alerts     AlertSettings `json:"alerts"`
	Notifiers  []Notifier    `json:"notifiers"`
}

// Default returns sensible defaults based on the international consensus
// target range of 70–180 mg/dL.
func Default() Settings {
	enabled := map[string]bool{}
	for _, k := range AllAlertKinds {
		enabled[k] = true
	}
	return Settings{
		Name:  "Tideline",
		Units: UnitsMgdl,
		Thresholds: Thresholds{
			UrgentLow:  55,
			Low:        70,
			High:       180,
			UrgentHigh: 250,
		},
		Alerts: AlertSettings{
			Enabled:             enabled,
			StaleMinutes:        15,
			UrgentStaleMinutes:  30,
			PredictMinutes:      20,
			RepeatWarnMinutes:   15,
			RepeatUrgentMinutes: 5,
		},
		Notifiers: []Notifier{},
	}
}

// Validate checks that the settings are internally consistent.
func (s Settings) Validate() error {
	t := s.Thresholds
	if !(t.UrgentLow > 0 && t.UrgentLow <= t.Low && t.Low < t.High && t.High <= t.UrgentHigh && t.UrgentHigh < 600) {
		return errors.New("thresholds must satisfy 0 < urgentLow <= low < high <= urgentHigh < 600 (mg/dL)")
	}
	if s.Units != UnitsMgdl && s.Units != UnitsMmol {
		return fmt.Errorf("units must be %q or %q", UnitsMgdl, UnitsMmol)
	}
	a := s.Alerts
	if a.StaleMinutes < 5 || a.UrgentStaleMinutes < a.StaleMinutes {
		return errors.New("staleMinutes must be >= 5 and urgentStaleMinutes >= staleMinutes")
	}
	if a.PredictMinutes < 5 || a.PredictMinutes > 60 {
		return errors.New("predictMinutes must be between 5 and 60")
	}
	if a.RepeatWarnMinutes < 1 || a.RepeatUrgentMinutes < 1 {
		return errors.New("repeat intervals must be at least 1 minute")
	}
	seen := map[string]bool{}
	for _, n := range s.Notifiers {
		if n.ID == "" || seen[n.ID] {
			return errors.New("each notifier needs a unique id")
		}
		seen[n.ID] = true
		switch n.Type {
		case "ntfy", "webhook":
			if n.URL == "" {
				return fmt.Errorf("notifier %q: url is required", n.Name)
			}
		case "pushover":
			if n.Token == "" || n.User == "" {
				return fmt.Errorf("notifier %q: token and user are required", n.Name)
			}
		default:
			return fmt.Errorf("notifier %q: unknown type %q", n.Name, n.Type)
		}
		if n.MinLevel != "warn" && n.MinLevel != "urgent" {
			return fmt.Errorf("notifier %q: minLevel must be warn or urgent", n.Name)
		}
		if n.EscalateAfterMinutes < 0 {
			return fmt.Errorf("notifier %q: escalateAfterMinutes must be >= 0", n.Name)
		}
	}
	return nil
}

// Public returns a copy safe to show to non-admin viewers (no notifier secrets).
func (s Settings) Public() Settings {
	c := s
	c.Notifiers = nil
	return c
}

// KV is the persistence the Manager needs.
type KV interface {
	GetSetting(ctx context.Context, key string) ([]byte, bool, error)
	PutSetting(ctx context.Context, key string, value []byte) error
}

const storeKey = "settings"

// Manager caches settings in memory and persists changes.
type Manager struct {
	mu        sync.RWMutex
	cur       Settings
	kv        KV
	listeners []func(Settings)
}

// NewManager loads settings from kv, falling back to defaults. Fields missing
// from a stored document keep their default values.
func NewManager(ctx context.Context, kv KV, defaults Settings) (*Manager, error) {
	m := &Manager{kv: kv, cur: defaults}
	raw, ok, err := kv.GetSetting(ctx, storeKey)
	if err != nil {
		return nil, err
	}
	if ok {
		s := defaults
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("decode stored settings: %w", err)
		}
		for _, k := range AllAlertKinds {
			if _, ok := s.Alerts.Enabled[k]; !ok {
				s.Alerts.Enabled[k] = true
			}
		}
		if s.Notifiers == nil {
			s.Notifiers = []Notifier{}
		}
		m.cur = s
	}
	return m, nil
}

// Get returns the current settings. The Enabled map and Notifiers slice are
// copies, so callers may not mutate the shared state.
func (m *Manager) Get() Settings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return clone(m.cur)
}

// Update validates and persists new settings, then notifies listeners.
func (m *Manager) Update(ctx context.Context, s Settings) error {
	if s.Notifiers == nil {
		s.Notifiers = []Notifier{}
	}
	if s.Alerts.Enabled == nil {
		s.Alerts.Enabled = map[string]bool{}
	}
	if err := s.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := m.kv.PutSetting(ctx, storeKey, raw); err != nil {
		return err
	}
	m.mu.Lock()
	m.cur = clone(s)
	ls := append([]func(Settings){}, m.listeners...)
	m.mu.Unlock()
	for _, l := range ls {
		l(clone(s))
	}
	return nil
}

// OnChange registers a callback invoked after each successful Update.
func (m *Manager) OnChange(f func(Settings)) {
	m.mu.Lock()
	m.listeners = append(m.listeners, f)
	m.mu.Unlock()
}

func clone(s Settings) Settings {
	c := s
	c.Alerts.Enabled = make(map[string]bool, len(s.Alerts.Enabled))
	for k, v := range s.Alerts.Enabled {
		c.Alerts.Enabled[k] = v
	}
	c.Notifiers = append([]Notifier{}, s.Notifiers...)
	return c
}
