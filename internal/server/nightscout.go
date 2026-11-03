package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/iamgp/tideline/internal/settings"
	"github.com/iamgp/tideline/internal/store"
)

// routesNightscout registers the Nightscout API v1 surface used by uploaders
// (xDrip+, Loop, Trio, Spike…) and followers (watch faces, widgets).
func (s *Server) routesNightscout(mux *http.ServeMux) {
	both := func(method, path string, h http.HandlerFunc) {
		mux.HandleFunc(method+" "+path, h)
		mux.HandleFunc(method+" "+path+".json", h)
	}
	both("GET", "/api/v1/status", s.handleStatus)
	mux.HandleFunc("GET /api/v1/verifyauth", s.handleVerifyAuth)

	both("GET", "/api/v1/entries", s.read(s.findHandler(store.Entries, "")))
	mux.HandleFunc("GET /api/v1/entries/{spec}", s.read(s.handleEntriesSpec))
	both("POST", "/api/v1/entries", s.write(s.insertHandler(s.store.InsertEntries)))
	mux.HandleFunc("DELETE /api/v1/entries/{id}", s.write(s.deleteHandler(store.Entries)))

	both("GET", "/api/v1/treatments", s.read(s.findHandler(store.Treatments, "")))
	both("POST", "/api/v1/treatments", s.write(s.insertHandler(s.store.InsertTreatments)))
	both("PUT", "/api/v1/treatments", s.write(s.handleUpdateTreatment))
	mux.HandleFunc("DELETE /api/v1/treatments/{id}", s.write(s.deleteHandler(store.Treatments)))

	both("GET", "/api/v1/devicestatus", s.read(s.findHandler(store.DeviceStatus, "")))
	both("POST", "/api/v1/devicestatus", s.write(s.insertHandler(s.store.InsertDeviceStatus)))
	mux.HandleFunc("DELETE /api/v1/devicestatus/{id}", s.write(s.deleteHandler(store.DeviceStatus)))

	both("GET", "/api/v1/profile", s.read(s.findHandler(store.Profiles, "")))
	both("POST", "/api/v1/profile", s.write(s.insertHandler(s.store.InsertProfiles)))
	both("PUT", "/api/v1/profile", s.write(s.insertHandler(s.store.InsertProfiles)))
	mux.HandleFunc("GET /api/v1/profile/current", s.read(s.handleCurrentProfile))

	both("GET", "/api/v1/food", s.read(s.findHandler(store.Food, "")))
	both("POST", "/api/v1/food", s.write(s.insertHandler(func(ctx context.Context, d []store.Doc) ([]store.Doc, error) {
		rs, err := s.store.Upsert(ctx, store.Food, d)
		out := make([]store.Doc, len(rs))
		for i, r := range rs {
			out[i] = r.Doc
		}
		return out, err
	})))
	mux.HandleFunc("DELETE /api/v1/food/{id}", s.write(s.deleteHandler(store.Food)))

	mux.HandleFunc("GET /pebble", s.read(s.handlePebble))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cur := s.settings.Get()
	now := time.Now().UTC()
	t := cur.Thresholds
	writeJSON(w, http.StatusOK, map[string]any{
		"status":            "ok",
		"name":              "tideline",
		"version":           s.opts.Version,
		"serverTime":        now.Format("2006-01-02T15:04:05.000Z"),
		"serverTimeEpoch":   now.UnixMilli(),
		"apiEnabled":        true,
		"careportalEnabled": true,
		"boluscalcEnabled":  false,
		"settings": map[string]any{
			"units":            cur.Units,
			"customTitle":      cur.Name,
			"timeFormat":       24,
			"authDefaultRoles": map[bool]string{true: "readable", false: "denied"}[s.publicRead()],
			"enable":           []string{"careportal", "iob", "cob", "basal", "devicestatus"},
			"thresholds": map[string]any{
				"bgHigh":         t.UrgentHigh,
				"bgTargetTop":    t.High,
				"bgTargetBottom": t.Low,
				"bgLow":          t.UrgentLow,
			},
		},
		"extendedSettings": map[string]any{},
		"authorized":       nil,
	})
}

func (s *Server) publicRead() bool { return s.auth.PublicRead() }

func (s *Server) handleVerifyAuth(w http.ResponseWriter, r *http.Request) {
	p, err := s.auth.Resolve(r)
	msg := map[string]any{"canRead": false, "canWrite": false, "isAdmin": false, "message": "UNAUTHORIZED", "rolefound": "NOTFOUND"}
	if err == nil && p != nil {
		msg = map[string]any{
			"canRead": p.CanRead(), "canWrite": p.CanWrite(), "isAdmin": p.IsAdmin(),
			"message": "OK", "rolefound": "FOUND", "permissions": "ROLE", "role": p.Role,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": 200, "message": msg})
}

func (s *Server) findHandler(coll, typ string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q, err := store.ParseQuery(r.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if typ != "" {
			q.Filters = append(q.Filters, store.Filter{Field: "type", Op: "$eq", Value: typ})
		}
		docs, err := s.store.Find(r.Context(), coll, q)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, docs)
	}
}

// handleEntriesSpec serves /api/v1/entries/{current|sgv|mbg|cal|<id>}[.json].
func (s *Server) handleEntriesSpec(w http.ResponseWriter, r *http.Request) {
	spec := strings.TrimSuffix(r.PathValue("spec"), ".json")
	switch spec {
	case "current":
		q, err := store.ParseQuery(r.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		q.Filters = append(q.Filters, store.Filter{Field: "type", Op: "$eq", Value: "sgv"})
		q.Count = 1
		docs, err := s.store.Find(r.Context(), store.Entries, q)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, docs)
	case "sgv", "mbg", "cal":
		s.findHandler(store.Entries, spec)(w, r)
	default:
		d, ok, err := s.store.Get(r.Context(), store.Entries, spec)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			writeJSON(w, http.StatusOK, []store.Doc{})
			return
		}
		writeJSON(w, http.StatusOK, []store.Doc{d})
	}
}

func (s *Server) insertHandler(insert func(context.Context, []store.Doc) ([]store.Doc, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := readBody(r)
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		docs, err := store.DecodeDocs(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if p := principal(r); p != nil {
			for _, d := range docs {
				if _, ok := d["enteredBy"]; !ok && strings.Contains(r.URL.Path, "treatments") {
					d["enteredBy"] = p.Name
				}
			}
		}
		out, err := insert(r.Context(), docs)
		if err != nil {
			s.log.Error("insert failed", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusInternalServerError, "insert failed")
			return
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) handleUpdateTreatment(w http.ResponseWriter, r *http.Request) {
	raw, err := readBody(r)
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, err.Error())
		return
	}
	docs, err := store.DecodeDocs(raw)
	if err != nil || len(docs) != 1 {
		writeError(w, http.StatusBadRequest, "expected a single treatment object")
		return
	}
	id, _ := docs[0]["_id"].(string)
	var d store.Doc
	ok := false
	if id != "" {
		d, ok, err = s.store.Replace(r.Context(), store.Treatments, id, docs[0])
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if !ok {
		// Nightscout upserts on PUT.
		out, err := s.store.InsertTreatments(r.Context(), docs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "insert failed")
			return
		}
		writeJSON(w, http.StatusOK, out[0])
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) deleteHandler(coll string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(r.PathValue("id"), ".json")
		ok, err := s.store.Delete(r.Context(), coll, id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		n := 0
		if ok {
			n = 1
			s.audit(r, "delete "+coll, id)
		}
		writeJSON(w, http.StatusOK, map[string]any{"n": n, "ok": 1})
	}
}

func (s *Server) handleCurrentProfile(w http.ResponseWriter, r *http.Request) {
	docs, err := s.store.Find(r.Context(), store.Profiles, store.Query{Count: 1})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(docs) == 0 {
		writeError(w, http.StatusNotFound, "no profile")
		return
	}
	writeJSON(w, http.StatusOK, docs[0])
}

var directionTrend = map[string]int{
	"NONE": 0, "DoubleUp": 1, "SingleUp": 2, "FortyFiveUp": 3, "Flat": 4,
	"FortyFiveDown": 5, "SingleDown": 6, "DoubleDown": 7, "NOT COMPUTABLE": 8, "RATE OUT OF RANGE": 9,
}

// handlePebble serves the legacy /pebble endpoint still used by many watch faces.
func (s *Server) handlePebble(w http.ResponseWriter, r *http.Request) {
	count := 1
	if c := r.URL.Query().Get("count"); c != "" {
		fmt.Sscan(c, &count)
	}
	count = max(1, min(count, 100))
	docs, err := s.store.Find(r.Context(), store.Entries, store.Query{
		Filters: []store.Filter{{Field: "type", Op: "$eq", Value: "sgv"}},
		Count:   count + 1,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	units := s.settings.Get().Units
	if u := r.URL.Query().Get("units"); u == settings.UnitsMmol || u == settings.UnitsMgdl {
		units = u
	}
	fmtv := func(v float64) string {
		if units == settings.UnitsMmol {
			return fmt.Sprintf("%.1f", v/18)
		}
		return fmt.Sprintf("%.0f", v)
	}
	num := func(d store.Doc, k string) float64 {
		var f float64
		fmt.Sscan(fmt.Sprint(d[k]), &f)
		return f
	}
	bgs := []map[string]any{}
	for i := 0; i < len(docs) && i < count; i++ {
		d := docs[i]
		dir, _ := d["direction"].(string)
		b := map[string]any{
			"sgv":       fmtv(num(d, "sgv")),
			"trend":     directionTrend[dir],
			"direction": dir,
			"datetime":  num(d, "date"),
		}
		if i+1 < len(docs) {
			delta := num(d, "sgv") - num(docs[i+1], "sgv")
			if units == settings.UnitsMmol {
				b["bgdelta"] = fmt.Sprintf("%.1f", delta/18)
			} else {
				b["bgdelta"] = fmt.Sprintf("%.0f", delta)
			}
		}
		bgs = append(bgs, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": []map[string]any{{"now": time.Now().UnixMilli()}},
		"bgs":    bgs,
		"cals":   []any{},
	})
}
