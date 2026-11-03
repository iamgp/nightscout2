package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/iamgp/tideline/internal/alerts"
	"github.com/iamgp/tideline/internal/auth"
	"github.com/iamgp/tideline/internal/settings"
	"github.com/iamgp/tideline/internal/stats"
	"github.com/iamgp/tideline/internal/store"
)

func intParam(r *http.Request, name string, def, lo, hi int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(name))
	if err != nil {
		return def
	}
	return max(lo, min(v, hi))
}

func (s *Server) settingsFor(p *auth.Principal) settings.Settings {
	cur := s.settings.Get()
	if p.IsAdmin() {
		return cur
	}
	return cur.Public()
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"principal": p,
		"canWrite":  p.CanWrite(),
		"isAdmin":   p.IsAdmin(),
		"settings":  s.settingsFor(p),
		"version":   s.opts.Version,
	})
}

// handleTimeline returns what the main screen needs in one round trip:
// glucose readings, treatments and the latest device status for a window.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	hours := intParam(r, "hours", 24, 1, 72)
	now := time.Now()
	from := now.Add(-time.Duration(hours) * time.Hour)
	readings, err := s.store.SGVs(r.Context(), from, now.Add(time.Hour))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if readings == nil {
		readings = []store.Reading{}
	}
	treatments, err := s.store.Find(r.Context(), store.Treatments, store.Query{
		Filters: []store.Filter{{Field: "created_at", Op: "$gte", Value: strconv.FormatInt(from.UnixMilli(), 10)}},
		Count:   2000,
		Asc:     true,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var status any
	if ds, err := s.store.Find(r.Context(), store.DeviceStatus, store.Query{Count: 1}); err == nil && len(ds) > 0 {
		status = ds[0]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"now":          now.UnixMilli(),
		"from":         from.UnixMilli(),
		"readings":     readings,
		"treatments":   treatments,
		"deviceStatus": status,
		"alerts":       s.alerts.Active(),
	})
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	days := intParam(r, "days", 14, 1, 90)
	loc := time.UTC
	if tz := r.URL.Query().Get("tz"); tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			writeError(w, http.StatusBadRequest, "unknown time zone "+tz)
			return
		}
		loc = l
	}
	to := time.Now()
	from := to.Add(-time.Duration(days) * 24 * time.Hour)
	rs, err := s.store.SGVs(r.Context(), from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats.Build(rs, from, to, loc))
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsFor(principal(r)))
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in settings.Settings
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid settings: "+err.Error())
		return
	}
	for i := range in.Notifiers {
		if in.Notifiers[i].ID == "" {
			in.Notifiers[i].ID = store.NewID()
		}
	}
	if err := s.settings.Update(r.Context(), in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "update settings", "")
	writeJSON(w, http.StatusOK, s.settings.Get())
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.alerts.Active())
}

func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Minutes int `json:"minutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "expected {\"minutes\": n}")
		return
	}
	if body.Minutes < 0 || body.Minutes > 24*60 {
		writeError(w, http.StatusBadRequest, "minutes must be between 0 and 1440")
		return
	}
	kind := r.PathValue("kind")
	if err := s.alerts.Snooze(kind, time.Duration(body.Minutes)*time.Minute); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.audit(r, "snooze alert", kind+" for "+strconv.Itoa(body.Minutes)+"m")
	writeJSON(w, http.StatusOK, s.alerts.Active())
}

func (s *Server) handleTestNotifier(w http.ResponseWriter, r *http.Request) {
	var n settings.Notifier
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		writeError(w, http.StatusBadRequest, "invalid notifier")
		return
	}
	if n.ID == "" {
		n.ID = "test"
	}
	if n.MinLevel == "" {
		n.MinLevel = alerts.LevelWarn
	}
	probe := settings.Default()
	probe.Notifiers = []settings.Notifier{n}
	if err := probe.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if s.opts.Sender == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications are not configured")
		return
	}
	err := s.opts.Sender.Send(r.Context(), n, alerts.Alert{
		Kind: "test", Level: alerts.LevelWarn, Title: "Test notification",
		Message: "If you can read this, Tideline can reach you.", Since: time.Now(),
	}, false)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.store.ListTokens(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, toks)
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string `json:"name"`
		Role          string `json:"role"`
		ExpiresInDays int    `json:"expiresInDays"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 64 {
		writeError(w, http.StatusBadRequest, "name is required (max 64 characters)")
		return
	}
	if !auth.ValidRole(body.Role) {
		writeError(w, http.StatusBadRequest, "role must be reader, uploader or admin")
		return
	}
	var exp *time.Time
	if body.ExpiresInDays > 0 {
		t := time.Now().Add(time.Duration(body.ExpiresInDays) * 24 * time.Hour)
		exp = &t
	}
	plain, tok, err := s.store.CreateToken(r.Context(), body.Name, body.Role, exp)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "create token", body.Name+" ("+body.Role+")")
	writeJSON(w, http.StatusCreated, map[string]any{"token": plain, "info": tok})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ok, err := s.store.RevokeToken(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "no active token with that id")
		return
	}
	s.audit(r, "revoke token", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	ev, err := s.store.AuditLog(r.Context(), intParam(r, "limit", 100, 1, 1000))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ev)
}
