// Package dexcom pulls CGM readings from Dexcom Share, so a Dexcom user can
// be followed without running a separate uploader app.
package dexcom

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iamgp/tideline/internal/store"
)

// Region base URLs for the Share web services.
var regions = map[string]string{
	"us":  "https://share2.dexcom.com/ShareWebServices/Services",
	"ous": "https://shareous1.dexcom.com/ShareWebServices/Services",
	"jp":  "https://share.dexcom.jp/ShareWebServices/Services",
}

// RegionURL returns the Share base URL for a region code (us, ous, jp).
func RegionURL(region string) (string, error) {
	u, ok := regions[strings.ToLower(region)]
	if !ok {
		return "", fmt.Errorf("unknown Dexcom region %q (want us, ous or jp)", region)
	}
	return u, nil
}

// applicationID identifies the Dexcom Share client (the same public ID the
// Nightscout share2 bridge uses).
const applicationID = "d89443d2-327c-4a6f-89e5-496bbb0317db"

// trends maps Dexcom's trend numbers to Nightscout direction names.
var trends = []string{
	"NONE", "DoubleUp", "SingleUp", "FortyFiveUp", "Flat",
	"FortyFiveDown", "SingleDown", "DoubleDown", "NOT COMPUTABLE", "RATE OUT OF RANGE",
}

var trendNames = map[string]string{
	"none": "NONE", "doubleup": "DoubleUp", "singleup": "SingleUp", "fortyfiveup": "FortyFiveUp",
	"flat": "Flat", "fortyfivedown": "FortyFiveDown", "singledown": "SingleDown",
	"doubledown": "DoubleDown", "notcomputable": "NOT COMPUTABLE", "rateoutofrange": "RATE OUT OF RANGE",
}

// Client talks to Dexcom Share.
type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client

	sessionID string
}

// Reading is one Share glucose value.
type Reading struct {
	Time      time.Time
	Value     int
	Direction string
}

type apiError struct {
	Code    string `json:"Code"`
	Message string `json:"Message"`
	status  int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("dexcom share: %s (%s, HTTP %d)", e.Message, e.Code, e.status)
}

func (c *Client) post(ctx context.Context, path string, body any, out any) error {
	var buf io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		buf = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/"+path, buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Dexcom Share/3.0.2.11 CFNetwork/711.2.23 Darwin/14.0.0")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		e := &apiError{status: resp.StatusCode}
		_ = json.Unmarshal(raw, e)
		if e.Code == "" {
			e.Code = "HTTPError"
			e.Message = strings.TrimSpace(string(raw))
		}
		return e
	}
	return json.Unmarshal(raw, out)
}

// Login authenticates and stores a session ID.
func (c *Client) Login(ctx context.Context) error {
	var accountID string
	if err := c.post(ctx, "General/AuthenticatePublisherAccount", map[string]string{
		"accountName": c.Username, "password": c.Password, "applicationId": applicationID,
	}, &accountID); err != nil {
		return err
	}
	var sid string
	if err := c.post(ctx, "General/LoginPublisherAccountById", map[string]string{
		"accountId": accountID, "password": c.Password, "applicationId": applicationID,
	}, &sid); err != nil {
		return err
	}
	if sid == "" || sid == "00000000-0000-0000-0000-000000000000" {
		return errors.New("dexcom share: login returned no session; check username, password and region")
	}
	c.sessionID = sid
	return nil
}

type shareValue struct {
	WT    string          `json:"WT"`
	Value int             `json:"Value"`
	Trend json.RawMessage `json:"Trend"`
}

var dateRe = regexp.MustCompile(`Date\((\d+)`)

func parseTrend(raw json.RawMessage) string {
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		if n >= 0 && n < len(trends) {
			return trends[n]
		}
		return "NONE"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if d, ok := trendNames[strings.ToLower(s)]; ok {
			return d
		}
	}
	return "NONE"
}

// Latest fetches up to maxCount readings from the last minutes, newest first.
// It logs in (again) when there's no session or the session has expired.
func (c *Client) Latest(ctx context.Context, minutes, maxCount int) ([]Reading, error) {
	if c.sessionID == "" {
		if err := c.Login(ctx); err != nil {
			return nil, err
		}
	}
	fetch := func() ([]shareValue, error) {
		var vals []shareValue
		path := fmt.Sprintf("Publisher/ReadPublisherLatestGlucoseValues?sessionId=%s&minutes=%d&maxCount=%d",
			c.sessionID, minutes, maxCount)
		return vals, c.post(ctx, path, nil, &vals)
	}
	vals, err := fetch()
	var ae *apiError
	if errors.As(err, &ae) && strings.Contains(ae.Code, "Session") {
		if err := c.Login(ctx); err != nil {
			return nil, err
		}
		vals, err = fetch()
	}
	if err != nil {
		return nil, err
	}
	out := make([]Reading, 0, len(vals))
	for _, v := range vals {
		m := dateRe.FindStringSubmatch(v.WT)
		if m == nil || v.Value <= 0 {
			continue
		}
		ms, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, Reading{Time: time.UnixMilli(ms).UTC(), Value: v.Value, Direction: parseTrend(v.Trend)})
	}
	return out, nil
}

// ToEntries converts readings to Nightscout entry documents.
func ToEntries(rs []Reading) []store.Doc {
	docs := make([]store.Doc, 0, len(rs))
	for _, r := range rs {
		docs = append(docs, store.Doc{
			"type":       "sgv",
			"sgv":        r.Value,
			"direction":  r.Direction,
			"date":       r.Time.UnixMilli(),
			"dateString": r.Time.Format("2006-01-02T15:04:05.000Z"),
			"device":     "share2",
		})
	}
	return docs
}

// Poll fetches readings every interval and passes them to insert. The first
// fetch backfills the last 24 hours.
func Poll(ctx context.Context, c *Client, interval time.Duration, insert func(context.Context, []store.Doc) error, log *slog.Logger) {
	var lastOK time.Time
	backoff := interval
	for {
		// Fetch enough history to cover any gap since the last success.
		minutes := 1440
		if !lastOK.IsZero() {
			minutes = max(30, min(1440, int(time.Since(lastOK).Minutes())+10))
		}
		rs, err := c.Latest(ctx, minutes, minutes/5+1)
		switch {
		case err != nil:
			log.Warn("dexcom share fetch failed", "err", err)
			backoff = min(backoff*2, 30*time.Minute)
		case len(rs) > 0:
			if err := insert(ctx, ToEntries(rs)); err != nil {
				log.Error("dexcom share insert failed", "err", err)
			} else {
				log.Debug("dexcom share fetched readings", "count", len(rs))
				lastOK = time.Now()
			}
			backoff = interval
		default:
			lastOK = time.Now()
			backoff = interval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}
