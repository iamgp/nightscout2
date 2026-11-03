// Package server wires Tideline's HTTP API, event stream and web UI together.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/iamgp/tideline/internal/alerts"
	"github.com/iamgp/tideline/internal/auth"
	"github.com/iamgp/tideline/internal/settings"
	"github.com/iamgp/tideline/internal/store"
)

// Options configures a Server.
type Options struct {
	Version   string
	PublicURL string
	Log       *slog.Logger
	Static    fs.FS // web UI assets; nil disables the UI
	Sender    alerts.Sender
}

// Server is the Tideline HTTP server.
type Server struct {
	opts     Options
	store    *store.Store
	auth     *auth.Authenticator
	settings *settings.Manager
	alerts   *alerts.Engine
	hub      *Hub
	log      *slog.Logger
	started  time.Time
}

// New builds a Server and connects store changes to the event stream and the
// alert engine.
func New(st *store.Store, a *auth.Authenticator, sm *settings.Manager, opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	s := &Server{
		opts: opts, store: st, auth: a, settings: sm,
		hub: NewHub(), log: opts.Log, started: time.Now(),
	}
	s.alerts = alerts.New(st, sm.Get, func(list []alerts.Alert) {
		s.hub.Publish(Event{"alerts", list})
	}, opts.Sender, opts.Log)

	st.OnChange(func(coll string, docs []store.Doc) {
		s.hub.Publish(Event{coll, docs})
		if coll == store.Entries {
			go s.alerts.Evaluate(context.Background())
		}
	})
	sm.OnChange(func(cur settings.Settings) {
		s.hub.Publish(Event{"settings", cur.Public()})
		go s.alerts.Evaluate(context.Background())
	})
	return s
}

// Alerts exposes the alert engine (for running its loop).
func (s *Server) Alerts() *alerts.Engine { return s.alerts }

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Nightscout API v1 compatibility.
	s.routesNightscout(mux)

	// Native API.
	mux.HandleFunc("GET /api/tl/v1/me", s.read(s.handleMe))
	mux.HandleFunc("GET /api/tl/v1/stream", s.read(s.handleStream))
	mux.HandleFunc("GET /api/tl/v1/timeline", s.read(s.handleTimeline))
	mux.HandleFunc("GET /api/tl/v1/report", s.read(s.handleReport))
	mux.HandleFunc("GET /api/tl/v1/settings", s.read(s.handleGetSettings))
	mux.HandleFunc("PUT /api/tl/v1/settings", s.admin(s.handlePutSettings))
	mux.HandleFunc("GET /api/tl/v1/alerts", s.read(s.handleAlerts))
	mux.HandleFunc("POST /api/tl/v1/alerts/{kind}/snooze", s.read(s.handleSnooze))
	mux.HandleFunc("POST /api/tl/v1/notifiers/test", s.admin(s.handleTestNotifier))
	mux.HandleFunc("GET /api/tl/v1/tokens", s.admin(s.handleListTokens))
	mux.HandleFunc("POST /api/tl/v1/tokens", s.admin(s.handleCreateToken))
	mux.HandleFunc("DELETE /api/tl/v1/tokens/{id}", s.admin(s.handleRevokeToken))
	mux.HandleFunc("GET /api/tl/v1/audit", s.admin(s.handleAudit))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": s.opts.Version})
	})

	if s.opts.Static != nil {
		files := http.FileServerFS(s.opts.Static)
		mux.Handle("GET /", uiHeaders(files))
	}
	return s.middleware(mux)
}

// ---------------------------------------------------------------------------
// Middleware and helpers

type ctxKey struct{}

func principal(r *http.Request) *auth.Principal {
	p, _ := r.Context().Value(ctxKey{}).(*auth.Principal)
	return p
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Nightscout clients (watch faces, web followers) call cross-origin.
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "api-secret, Authorization, Content-Type, Accept")
		h.Set("X-Content-Type-Options", "nosniff")
		// Tokens can appear in URLs (?token=) for follower links; never leak them.
		h.Set("Referrer-Policy", "no-referrer")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "err", rec, "stack", string(debug.Stack()))
				http.Error(sw, "internal error", http.StatusInternalServerError)
			}
			if strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasSuffix(r.URL.Path, "/stream") {
				s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
					"dur", time.Since(start).Round(time.Millisecond))
			}
		}()
		if r.Body != nil {
			r.Body = http.MaxBytesReader(sw, r.Body, 16<<20)
		}
		next.ServeHTTP(sw, r)
	})
}

func uiHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; connect-src 'self'; media-src 'self' data:; frame-ancestors 'none'; base-uri 'none'")
		h.Set("X-Frame-Options", "DENY")
		if r.URL.Path == "/sw.js" || r.URL.Path == "/" || r.URL.Path == "/index.html" {
			h.Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

type need int

const (
	needRead need = iota
	needWrite
	needAdmin
)

func (s *Server) guard(n need, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Resolve(r)
		if err != nil {
			var bad auth.ErrBadCredentials
			if errors.As(err, &bad) {
				writeError(w, http.StatusUnauthorized, "invalid credentials")
				return
			}
			s.log.Error("auth failed", "err", err)
			writeError(w, http.StatusInternalServerError, "auth failed")
			return
		}
		ok := false
		switch n {
		case needRead:
			ok = p.CanRead()
		case needWrite:
			ok = p.CanWrite()
		case needAdmin:
			ok = p.IsAdmin()
		}
		if !ok {
			if p == nil {
				writeError(w, http.StatusUnauthorized, "authentication required")
			} else {
				writeError(w, http.StatusForbidden, "not allowed for role "+string(p.Role))
			}
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	}
}

func (s *Server) read(h http.HandlerFunc) http.HandlerFunc  { return s.guard(needRead, h) }
func (s *Server) write(h http.HandlerFunc) http.HandlerFunc { return s.guard(needWrite, h) }
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc { return s.guard(needAdmin, h) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"status": status, "message": msg})
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

func (s *Server) audit(r *http.Request, action, detail string) {
	actor := "unknown"
	if p := principal(r); p != nil {
		actor = p.Name
	}
	if err := s.store.Audit(r.Context(), actor, action, detail); err != nil {
		s.log.Warn("audit write failed", "err", err)
	}
}
