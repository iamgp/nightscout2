package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iamgp/tideline/internal/alerts"
	"github.com/iamgp/tideline/internal/settings"
	"github.com/iamgp/tideline/internal/store"
	"github.com/iamgp/tideline/internal/webpush"
)

// pushServiceSuffixes are the hosts browsers use for Web Push. Registering
// any other endpoint is refused, so a follower can't make the server send
// requests to arbitrary (e.g. internal) addresses.
var pushServiceSuffixes = []string{
	".googleapis.com",            // Chrome, Edge on Android, most Chromium browsers (FCM)
	".push.services.mozilla.com", // Firefox
	".push.apple.com",            // Safari, iOS home-screen apps
	".notify.windows.com",        // Edge on Windows (WNS)
}

// DefaultPushHostAllowed reports whether host belongs to a known push service.
func DefaultPushHostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, s := range pushServiceSuffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return false
}

func (s *Server) routesPush(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/tl/v1/push/key", s.read(s.handlePushKey))
	mux.HandleFunc("GET /api/tl/v1/push/subscriptions", s.read(s.handleListPush))
	mux.HandleFunc("POST /api/tl/v1/push/subscriptions", s.read(s.handleSubscribePush))
	mux.HandleFunc("DELETE /api/tl/v1/push/subscriptions/{id}", s.read(s.handleDeletePush))
	mux.HandleFunc("POST /api/tl/v1/push/subscriptions/{id}/test", s.read(s.handleTestPush))
}

// pushNotifiers turns active device registrations into alert notifiers.
func (s *Server) pushNotifiers() []settings.Notifier {
	if s.opts.Push == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	subs, err := s.store.ActivePushSubscriptions(ctx)
	if err != nil {
		s.log.Error("load push subscriptions", "err", err)
		return nil
	}
	out := make([]settings.Notifier, 0, len(subs))
	for _, p := range subs {
		out = append(out, pushNotifier(p))
	}
	return out
}

func pushNotifier(p store.PushSubscription) settings.Notifier {
	return settings.Notifier{
		ID: "push:" + p.ID, Name: p.Name + " (" + p.Owner + ")", Type: "webpush",
		URL: p.Endpoint, Token: p.P256dh, User: p.Auth,
		MinLevel: p.MinLevel, EscalateAfterMinutes: p.EscalateAfterMinutes, Enabled: true,
	}
}

func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	if s.opts.Push == nil {
		writeError(w, http.StatusServiceUnavailable, "web push is not configured")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"publicKey": s.opts.Push.Keys.PublicKey()})
}

// canManage reports whether the caller owns a subscription or is an admin.
func canManage(r *http.Request, p store.PushSubscription) bool {
	me := principal(r)
	return me.IsAdmin() || (me.TokenID != "" && me.TokenID == p.TokenID)
}

func (s *Server) handleListPush(w http.ResponseWriter, r *http.Request) {
	subs, err := s.store.ListPushSubscriptions(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	me := principal(r)
	out := []store.PushSubscription{}
	for _, p := range subs {
		p.Mine = me.TokenID != "" && p.TokenID == me.TokenID || me.TokenID == "" && p.TokenID == ""
		if p.Mine || me.IsAdmin() {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleSubscribePush(w http.ResponseWriter, r *http.Request) {
	if s.opts.Push == nil {
		writeError(w, http.StatusServiceUnavailable, "web push is not configured")
		return
	}
	var body struct {
		Subscription struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
		} `json:"subscription"`
		Name                 string `json:"name"`
		MinLevel             string `json:"minLevel"`
		EscalateAfterMinutes int    `json:"escalateAfterMinutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	sub := body.Subscription
	u, err := url.Parse(sub.Endpoint)
	allowed := s.opts.PushHostAllowed
	if allowed == nil {
		allowed = DefaultPushHostAllowed
	}
	if err != nil || (u.Scheme != "https" && !s.opts.PushAllowHTTP) || !allowed(u.Hostname()) {
		writeError(w, http.StatusBadRequest, "endpoint is not a recognised push service")
		return
	}
	if sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		writeError(w, http.StatusBadRequest, "subscription keys are missing")
		return
	}
	// Check the keys are usable before storing them.
	if _, err := webpush.Encrypt(webpush.Subscription{Endpoint: sub.Endpoint, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth}, []byte("probe")); err != nil {
		writeError(w, http.StatusBadRequest, "invalid subscription keys: "+err.Error())
		return
	}
	if body.MinLevel != alerts.LevelUrgent {
		body.MinLevel = alerts.LevelWarn
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 64 {
		name = "Browser"
	}
	me := principal(r)
	p, err := s.store.SavePushSubscription(r.Context(), store.PushSubscription{
		Endpoint: sub.Endpoint, P256dh: sub.Keys.P256dh, Auth: sub.Keys.Auth,
		TokenID: me.TokenID, Owner: me.Name, Name: name,
		MinLevel: body.MinLevel, EscalateAfterMinutes: max(0, min(body.EscalateAfterMinutes, 240)),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	p.Mine = true
	s.audit(r, "register push device", name)
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) loadOwnPush(w http.ResponseWriter, r *http.Request) (store.PushSubscription, bool) {
	p, ok, err := s.store.GetPushSubscription(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return p, false
	}
	if !ok || !canManage(r, p) {
		writeError(w, http.StatusNotFound, "no such device")
		return p, false
	}
	return p, true
}

func (s *Server) handleDeletePush(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwnPush(w, r)
	if !ok {
		return
	}
	if _, err := s.store.DeletePushSubscription(r.Context(), p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "remove push device", p.Name+" ("+p.Owner+")")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleTestPush(w http.ResponseWriter, r *http.Request) {
	p, ok := s.loadOwnPush(w, r)
	if !ok {
		return
	}
	if s.opts.Sender == nil {
		writeError(w, http.StatusServiceUnavailable, "notifications are not configured")
		return
	}
	err := s.opts.Sender.Send(r.Context(), pushNotifier(p), alerts.Alert{
		Kind: "test", Level: alerts.LevelWarn, Title: "Test notification",
		Message: "Alerts will arrive on this device.", Since: time.Now(),
	}, false)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
