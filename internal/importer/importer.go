// Package importer copies history from an existing Nightscout site into
// Tideline, so switching doesn't mean losing years of data.
package importer

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/iamgp/tideline/internal/store"
)

// Options configures an import.
type Options struct {
	Source     string // base URL of the Nightscout site
	Token      string // access token with read permission, or
	Secret     string // the site's API secret
	Days       int
	Chunk      time.Duration // window size per request (default 7 days)
	HTTP       *http.Client
	Logf       func(format string, args ...any)
	SkipStatus bool // skip devicestatus, which can be very large
}

// Counts reports how many documents were imported.
type Counts struct {
	Entries      int `json:"entries"`
	Treatments   int `json:"treatments"`
	DeviceStatus int `json:"devicestatus"`
	Profiles     int `json:"profiles"`
}

// Writer is the subset of the store the importer uses.
type Writer interface {
	InsertEntries(ctx context.Context, docs []store.Doc) ([]store.Doc, error)
	InsertTreatments(ctx context.Context, docs []store.Doc) ([]store.Doc, error)
	InsertDeviceStatus(ctx context.Context, docs []store.Doc) ([]store.Doc, error)
	InsertProfiles(ctx context.Context, docs []store.Doc) ([]store.Doc, error)
}

// Run performs the import.
func Run(ctx context.Context, w Writer, o Options) (Counts, error) {
	var c Counts
	if o.Days <= 0 {
		o.Days = 90
	}
	if o.Chunk <= 0 {
		o.Chunk = 7 * 24 * time.Hour
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 2 * time.Minute}
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	base, err := url.Parse(strings.TrimRight(o.Source, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return c, fmt.Errorf("invalid source URL %q", o.Source)
	}

	get := func(path string, q url.Values) ([]store.Doc, error) {
		u := *base
		u.Path = strings.TrimRight(u.Path, "/") + path
		if o.Token != "" {
			q.Set("token", o.Token)
		}
		u.RawQuery = q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if o.Secret != "" {
			h := sha1.Sum([]byte(o.Secret))
			req.Header.Set("api-secret", hex.EncodeToString(h[:]))
		}
		resp, err := o.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GET %s: %s", path, resp.Status)
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return nil, nil
		}
		return store.DecodeDocs(raw)
	}

	end := time.Now().UTC()
	start := end.Add(-time.Duration(o.Days) * 24 * time.Hour)
	iso := func(t time.Time) string { return t.Format("2006-01-02T15:04:05.000Z") }

	for from := start; from.Before(end); from = from.Add(o.Chunk) {
		to := from.Add(o.Chunk)
		if to.After(end) {
			to = end.Add(time.Minute)
		}
		o.Logf("importing %s → %s", from.Format("2006-01-02"), to.Format("2006-01-02"))

		docs, err := get("/api/v1/entries.json", url.Values{
			"find[date][$gte]": {fmt.Sprint(from.UnixMilli())},
			"find[date][$lt]":  {fmt.Sprint(to.UnixMilli())},
			"count":            {"100000"},
		})
		if err != nil {
			return c, err
		}
		if len(docs) > 0 {
			if _, err := w.InsertEntries(ctx, docs); err != nil {
				return c, fmt.Errorf("store entries: %w", err)
			}
			c.Entries += len(docs)
		}

		docs, err = get("/api/v1/treatments.json", url.Values{
			"find[created_at][$gte]": {iso(from)},
			"find[created_at][$lt]":  {iso(to)},
			"count":                  {"100000"},
		})
		if err != nil {
			return c, err
		}
		if len(docs) > 0 {
			if _, err := w.InsertTreatments(ctx, docs); err != nil {
				return c, fmt.Errorf("store treatments: %w", err)
			}
			c.Treatments += len(docs)
		}

		if !o.SkipStatus {
			docs, err = get("/api/v1/devicestatus.json", url.Values{
				"find[created_at][$gte]": {iso(from)},
				"find[created_at][$lt]":  {iso(to)},
				"count":                  {"100000"},
			})
			if err != nil {
				return c, err
			}
			if len(docs) > 0 {
				if _, err := w.InsertDeviceStatus(ctx, docs); err != nil {
					return c, fmt.Errorf("store devicestatus: %w", err)
				}
				c.DeviceStatus += len(docs)
			}
		}
	}

	docs, err := get("/api/v1/profile.json", url.Values{})
	if err != nil {
		return c, err
	}
	if len(docs) > 0 {
		if _, err := w.InsertProfiles(ctx, docs); err != nil {
			return c, fmt.Errorf("store profiles: %w", err)
		}
		c.Profiles = len(docs)
	}
	return c, nil
}
