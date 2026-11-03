package server

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iamgp/tideline/internal/auth"
	"github.com/iamgp/tideline/internal/store"
)

// Nightscout API v3, as used by AndroidAPS (NSClient v3) and other modern
// clients. Clients sync incrementally through /history/{lastModified}, which
// relies on the store's strictly increasing srvModified and soft deletes.
//
// The version reported to clients is the Nightscout release whose API
// Tideline implements, since clients gate features on it.
const (
	nsCompatVersion = "15.0.3"
	nsAPIVersion    = "3.0.4"
	v3MaxLimit      = 1000
)

func (s *Server) routesV3(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v3/version", s.handleV3Version)
	mux.HandleFunc("GET /api/v3/status", s.read(s.handleV3Status))
	mux.HandleFunc("GET /api/v3/lastModified", s.read(s.handleV3LastModified))

	mux.HandleFunc("GET /api/v3/{coll}", s.read(s.v3(s.handleV3Search)))
	mux.HandleFunc("POST /api/v3/{coll}", s.write(s.v3(s.handleV3Create)))
	mux.HandleFunc("GET /api/v3/{coll}/history/{since}", s.read(s.v3(s.handleV3History)))
	mux.HandleFunc("GET /api/v3/{coll}/{id}", s.read(s.v3(s.handleV3Read)))
	mux.HandleFunc("PUT /api/v3/{coll}/{id}", s.write(s.v3(s.handleV3Update)))
	mux.HandleFunc("PATCH /api/v3/{coll}/{id}", s.write(s.v3(s.handleV3Patch)))
	mux.HandleFunc("DELETE /api/v3/{coll}/{id}", s.write(s.v3(s.handleV3Delete)))

	// API v2 authorization, which v3 clients use to trade an access token for a JWT.
	mux.HandleFunc("GET /api/v2/authorization/request/{token}", s.handleV2AuthRequest)
	mux.HandleFunc("GET /api/v2/authorization/debug/check/{permission}", s.read(s.handleV2PermissionCheck))
}

// v3 rejects unknown collections before the handler runs.
func (s *Server) v3(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(store.Collections, r.PathValue("coll")) {
			writeError(w, http.StatusNotFound, "collection not found")
			return
		}
		h(w, r)
	}
}

func v3OK(w http.ResponseWriter, status int, result any) {
	writeJSON(w, status, map[string]any{"status": status, "result": result})
}

func permissions(p *auth.Principal) string {
	switch {
	case p.CanWrite():
		return "crud"
	case p.CanRead():
		return "r"
	}
	return ""
}

func (s *Server) handleV3Version(w http.ResponseWriter, r *http.Request) {
	v3OK(w, http.StatusOK, map[string]any{
		"version":    nsCompatVersion,
		"apiVersion": nsAPIVersion,
		"srvDate":    time.Now().UnixMilli(),
		"storage":    map[string]string{"storage": "sqlite", "version": "3"},
		"tideline":   s.opts.Version,
	})
}

func (s *Server) handleV3Status(w http.ResponseWriter, r *http.Request) {
	perm := permissions(principal(r))
	perms := map[string]string{}
	for _, c := range store.Collections {
		perms[c] = perm
	}
	v3OK(w, http.StatusOK, map[string]any{
		"version":        nsCompatVersion,
		"apiVersion":     nsAPIVersion,
		"srvDate":        time.Now().UnixMilli(),
		"storage":        map[string]string{"storage": "sqlite", "version": "3"},
		"apiPermissions": perms,
		"tideline":       s.opts.Version,
	})
}

func (s *Server) handleV3LastModified(w http.ResponseWriter, r *http.Request) {
	lm, err := s.store.LastModified(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	cols := map[string]int64{}
	for k, v := range lm {
		cols[k] = v
	}
	cols["foods"] = lm[store.Food] // AndroidAPS reads the plural
	v3OK(w, http.StatusOK, map[string]any{"srvDate": time.Now().UnixMilli(), "collections": cols})
}

func (s *Server) handleV3Search(w http.ResponseWriter, r *http.Request) {
	q, err := store.ParseQueryV3(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q.Count = min(q.Count, v3MaxLimit)
	docs, err := s.store.Find(r.Context(), r.PathValue("coll"), q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	v3OK(w, http.StatusOK, docs)
}

func (s *Server) handleV3History(w http.ResponseWriter, r *http.Request) {
	since, err := strconv.ParseInt(r.PathValue("since"), 10, 64)
	if err != nil || since < 0 {
		writeError(w, http.StatusBadRequest, "lastModified must be epoch milliseconds")
		return
	}
	limit := v3MaxLimit
	if l := r.URL.Query().Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "invalid limit")
			return
		}
		limit = min(n, v3MaxLimit)
	}
	docs, err := s.store.History(r.Context(), r.PathValue("coll"), since, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	last := since
	for _, d := range docs {
		if m, ok := d["srvModified"].(int64); ok && m > last {
			last = m
		}
	}
	// Clients resume from the ETag (format W/"<ms>").
	w.Header().Set("ETag", fmt.Sprintf(`W/"%d"`, last))
	if last > 0 {
		w.Header().Set("Last-Modified", time.UnixMilli(last).UTC().Format(http.TimeFormat))
	}
	v3OK(w, http.StatusOK, docs)
}

func (s *Server) handleV3Read(w http.ResponseWriter, r *http.Request) {
	d, deleted, ok, err := s.store.GetAny(r.Context(), r.PathValue("coll"), r.PathValue("id"))
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	case !ok:
		writeError(w, http.StatusNotFound, "document not found")
	case deleted:
		writeError(w, http.StatusGone, "document has been deleted")
	default:
		v3OK(w, http.StatusOK, d)
	}
}

func decodeOne(r *http.Request) (store.Doc, error) {
	raw, err := readBody(r)
	if err != nil {
		return nil, err
	}
	docs, err := store.DecodeDocs(raw)
	if err != nil {
		return nil, err
	}
	if len(docs) != 1 {
		return nil, errors.New("expected a single JSON object")
	}
	return docs[0], nil
}

// writeV3Change answers a create/update/delete in the shape both Nightscout
// and AndroidAPS expect. AndroidAPS reads the fields at the top level for some
// calls and inside "result" for others, so they are sent in both places.
func writeV3Change(w http.ResponseWriter, status int, id string, lastModified int64, dedup bool) {
	res := map[string]any{"identifier": id, "lastModified": lastModified}
	if dedup {
		res["isDeduplication"] = true
		res["deduplicatedIdentifier"] = id
	}
	body := map[string]any{"status": status, "result": res}
	for k, v := range res {
		body[k] = v
	}
	writeJSON(w, status, body)
}

func (s *Server) handleV3Create(w http.ResponseWriter, r *http.Request) {
	d, err := decodeOne(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if _, ok := d["subject"]; !ok {
		d["subject"] = principal(r).Name
	}
	rs, err := s.store.Upsert(r.Context(), r.PathValue("coll"), []store.Doc{d})
	if err != nil {
		s.log.Error("v3 create failed", "coll", r.PathValue("coll"), "err", err)
		writeError(w, http.StatusInternalServerError, "create failed")
		return
	}
	res := rs[0]
	status := http.StatusCreated
	if res.Deduplicated {
		status = http.StatusOK
	}
	writeV3Change(w, status, res.Doc["identifier"].(string), res.Doc["srvModified"].(int64), res.Deduplicated)
}

func (s *Server) handleV3Update(w http.ResponseWriter, r *http.Request) {
	coll, id := r.PathValue("coll"), r.PathValue("id")
	d, err := decodeOne(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	_, deleted, exists, err := s.store.GetAny(r.Context(), coll, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if deleted {
		writeError(w, http.StatusGone, "document has been deleted")
		return
	}
	d["modifiedBy"] = principal(r).Name
	if !exists {
		// PUT to a new identifier creates the document.
		d["identifier"] = id
		rs, err := s.store.Upsert(r.Context(), coll, []store.Doc{d})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "create failed")
			return
		}
		writeV3Change(w, http.StatusCreated, rs[0].Doc["identifier"].(string), rs[0].Doc["srvModified"].(int64), rs[0].Deduplicated)
		return
	}
	out, _, err := s.store.Replace(r.Context(), coll, id, d)
	s.finishUpdate(w, out, err)
}

func (s *Server) handleV3Patch(w http.ResponseWriter, r *http.Request) {
	coll, id := r.PathValue("coll"), r.PathValue("id")
	patch, err := decodeOne(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	_, deleted, exists, err := s.store.GetAny(r.Context(), coll, id)
	switch {
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	case !exists:
		writeError(w, http.StatusNotFound, "document not found")
		return
	case deleted:
		writeError(w, http.StatusGone, "document has been deleted")
		return
	}
	for _, k := range []string{"identifier", "_id", "srvModified", "srvCreated", "isValid"} {
		delete(patch, k)
	}
	patch["modifiedBy"] = principal(r).Name
	out, _, err := s.store.Patch(r.Context(), coll, id, patch)
	s.finishUpdate(w, out, err)
}

func (s *Server) finishUpdate(w http.ResponseWriter, out store.Doc, err error) {
	switch {
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "update failed")
	case out == nil:
		writeError(w, http.StatusNotFound, "document not found")
	default:
		writeV3Change(w, http.StatusOK, out["identifier"].(string), out["srvModified"].(int64), false)
	}
}

func (s *Server) handleV3Delete(w http.ResponseWriter, r *http.Request) {
	coll, id := r.PathValue("coll"), r.PathValue("id")
	ok, err := s.store.Delete(r.Context(), coll, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "document not found")
		return
	}
	s.audit(r, "delete "+coll, id)
	writeJSON(w, http.StatusOK, map[string]any{"status": http.StatusOK, "identifier": id, "result": map[string]any{"identifier": id}})
}

func (s *Server) handleV2AuthRequest(w http.ResponseWriter, r *http.Request) {
	cred := "t:" + r.PathValue("token")
	if s.checkBlocked(w, r, cred) {
		return
	}
	jwt, p, iat, exp, err := s.auth.IssueJWT(r.Context(), r.PathValue("token"))
	if err != nil {
		var bad auth.ErrBadCredentials
		if errors.As(err, &bad) {
			s.authFailed(r)
			writeError(w, http.StatusUnauthorized, "invalid access token")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not issue token")
		return
	}
	s.limiter.Succeeded(cred)
	groups := [][]string{{"api:*:read"}}
	if p.CanWrite() {
		groups = [][]string{{"*"}}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token": jwt, "sub": p.Name, "permissionGroups": groups,
		"iat": iat.Unix(), "exp": exp.Unix(),
	})
}

// handleV2PermissionCheck answers whether the caller holds a Shiro-style
// permission such as "api:treatments:create".
func (s *Server) handleV2PermissionCheck(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	perm := r.PathValue("permission")
	op := perm[strings.LastIndex(perm, ":")+1:]
	allowed := p.IsAdmin() || (op == "read" && p.CanRead()) ||
		(strings.HasPrefix(perm, "api:") && slices.Contains([]string{"create", "update", "delete", "*"}, op) && p.CanWrite())
	if !allowed {
		writeError(w, http.StatusForbidden, "missing permission "+perm)
		return
	}
	v3OK(w, http.StatusOK, map[string]any{"permission": perm, "granted": true})
}
