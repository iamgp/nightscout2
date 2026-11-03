// Package libre pulls CGM readings from LibreLinkUp, the Abbott follower
// service, so FreeStyle Libre users can be followed without an uploader app.
//
// LibreLinkUp has no public API; this follows the protocol used by the
// widely deployed nightscout-librelink-up bridge. Abbott changes it from
// time to time, most often by requiring a newer app version header, which
// can be overridden with LIBRE_VERSION.
package libre

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/iamgp/tideline/internal/store"
)

// DefaultVersion is the LibreLinkUp app version sent with requests.
const DefaultVersion = "4.16.0"

// GlobalHost is the entry point; it redirects accounts to their regional host.
const GlobalHost = "api.libreview.io"

var regions = map[string]string{
	"ae": "api-ae.libreview.io", "ap": "api-ap.libreview.io", "au": "api-au.libreview.io",
	"ca": "api-ca.libreview.io", "de": "api-de.libreview.io", "eu": "api-eu.libreview.io",
	"eu2": "api-eu2.libreview.io", "fr": "api-fr.libreview.io", "jp": "api-jp.libreview.io",
	"us": "api-us.libreview.io", "la": "api-la.libreview.io", "ru": "api.libreview.ru",
}

// RegionHost returns the API host for a region code, or the global host
// (which redirects automatically) for "" or "auto".
func RegionHost(region string) (string, error) {
	r := strings.ToLower(strings.TrimSpace(region))
	if r == "" || r == "auto" {
		return GlobalHost, nil
	}
	if h, ok := regions[r]; ok {
		return h, nil
	}
	return "", fmt.Errorf("unknown LibreLinkUp region %q", region)
}

const userAgent = "Mozilla/5.0 (iPhone; CPU OS 17_4.1 like Mac OS X) AppleWebKit/536.26 (KHTML, like Gecko) Version/17.4.1 Mobile/10A5355d Safari/8536.25"

// Client talks to LibreLinkUp.
type Client struct {
	Host      string // e.g. api-eu.libreview.io; updated after a region redirect
	Email     string
	Password  string
	PatientID string // which connection to follow; "" picks the first
	Version   string
	HTTP      *http.Client
	// Scheme is "https" unless overridden for tests.
	Scheme string

	token     string
	expires   time.Time
	accountID string // SHA-256 of the user ID, required since app 4.11
}

// Reading is one glucose value.
type Reading struct {
	Time      time.Time
	Value     float64
	Direction string
}

// Error is a LibreLinkUp error the user can act on.
type Error struct{ Msg string }

func (e *Error) Error() string { return "librelinkup: " + e.Msg }

var errUnauthorized = errors.New("librelinkup: unauthorized")

func (c *Client) do(ctx context.Context, method, path string, body any, auth bool, out any) error {
	var rd io.Reader = http.NoBody
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	scheme := c.Scheme
	if scheme == "" {
		scheme = "https"
	}
	req, err := http.NewRequestWithContext(ctx, method, scheme+"://"+c.Host+path, rd)
	if err != nil {
		return err
	}
	version := c.Version
	if version == "" {
		version = DefaultVersion
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("version", version)
	req.Header.Set("product", "llu.ios")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("account-id", c.accountID)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		return &Error{"access denied (HTTP 403). LibreLinkUp may need a newer app version: set LIBRE_VERSION"}
	case resp.StatusCode == http.StatusTooManyRequests:
		return &Error{"rate limited by LibreLinkUp (HTTP 429)"}
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("librelinkup: %s %s: %s", method, path, resp.Status)
	}
	return json.Unmarshal(raw, out)
}

type loginResponse struct {
	Status int `json:"status"`
	Data   struct {
		Redirect bool   `json:"redirect"`
		Region   string `json:"region"`
		User     struct {
			ID string `json:"id"`
		} `json:"user"`
		AuthTicket struct {
			Token   string `json:"token"`
			Expires int64  `json:"expires"`
		} `json:"authTicket"`
		Step struct {
			Type string `json:"type"`
		} `json:"step"`
	} `json:"data"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Login authenticates, following a region redirect if the account lives
// elsewhere.
func (c *Client) Login(ctx context.Context) error {
	for attempt := 0; attempt < 2; attempt++ {
		var lr loginResponse
		if err := c.do(ctx, http.MethodPost, "/llu/auth/login",
			map[string]string{"email": c.Email, "password": c.Password}, false, &lr); err != nil {
			if errors.Is(err, errUnauthorized) {
				return &Error{"login rejected: check LIBRE_EMAIL and LIBRE_PASSWORD"}
			}
			return err
		}
		if lr.Data.Redirect && lr.Data.Region != "" {
			host, err := RegionHost(lr.Data.Region)
			if err != nil {
				return err
			}
			c.Host = host
			continue
		}
		switch {
		case lr.Status == 2:
			return &Error{"login rejected: check LIBRE_EMAIL and LIBRE_PASSWORD"}
		case lr.Status == 4 || lr.Data.Step.Type != "":
			return &Error{fmt.Sprintf("LibreLinkUp needs you to accept updated terms (%s): open the LibreLinkUp app, sign in and accept them", lr.Data.Step.Type)}
		case lr.Status != 0:
			msg := lr.Error.Message
			if msg == "" {
				msg = fmt.Sprintf("status %d", lr.Status)
			}
			return &Error{"login failed: " + msg}
		case lr.Data.AuthTicket.Token == "" || lr.Data.User.ID == "":
			return &Error{"login returned no session"}
		}
		c.token = lr.Data.AuthTicket.Token
		c.expires = time.Unix(lr.Data.AuthTicket.Expires, 0)
		sum := sha256.Sum256([]byte(lr.Data.User.ID))
		c.accountID = hex.EncodeToString(sum[:])
		return nil
	}
	return &Error{"too many region redirects"}
}

type measurement struct {
	FactoryTimestamp string  `json:"FactoryTimestamp"`
	ValueInMgPerDl   float64 `json:"ValueInMgPerDl"`
	TrendArrow       int     `json:"TrendArrow"`
}

type connection struct {
	PatientID          string      `json:"patientId"`
	GlucoseMeasurement measurement `json:"glucoseMeasurement"`
}

func trend(arrow int) string {
	switch arrow {
	case 1:
		return "SingleDown"
	case 2:
		return "FortyFiveDown"
	case 3:
		return "Flat"
	case 4:
		return "FortyFiveUp"
	case 5:
		return "SingleUp"
	}
	return "NOT COMPUTABLE"
}

// FactoryTimestamp is UTC, in US format, e.g. "11/3/2026 7:35:00 PM".
func parseFactory(s string) (time.Time, bool) {
	for _, l := range []string{"1/2/2006 3:04:05 PM", "1/2/2006 15:04:05"} {
		if t, err := time.Parse(l, strings.TrimSpace(s)); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

func (c *Client) authed(ctx context.Context, path string, out any) error {
	if c.token == "" || time.Now().After(c.expires.Add(-time.Minute)) {
		if err := c.Login(ctx); err != nil {
			return err
		}
	}
	err := c.do(ctx, http.MethodGet, path, nil, true, out)
	if errors.Is(err, errUnauthorized) {
		if err := c.Login(ctx); err != nil {
			return err
		}
		err = c.do(ctx, http.MethodGet, path, nil, true, out)
	}
	return err
}

// Latest returns the current reading (with trend) and the recent history.
func (c *Client) Latest(ctx context.Context) ([]Reading, error) {
	var conns struct {
		Data []connection `json:"data"`
	}
	if err := c.authed(ctx, "/llu/connections", &conns); err != nil {
		return nil, err
	}
	if len(conns.Data) == 0 {
		return nil, &Error{"no one is sharing with this account: in the Libre app, connect it to LibreLinkUp"}
	}
	patient := conns.Data[0].PatientID
	if c.PatientID != "" {
		patient = ""
		for _, cn := range conns.Data {
			if cn.PatientID == c.PatientID {
				patient = cn.PatientID
			}
		}
		if patient == "" {
			return nil, &Error{"LIBRE_PATIENT_ID does not match any connection on this account"}
		}
	}
	var graph struct {
		Data struct {
			Connection connection    `json:"connection"`
			GraphData  []measurement `json:"graphData"`
		} `json:"data"`
	}
	if err := c.authed(ctx, "/llu/connections/"+patient+"/graph", &graph); err != nil {
		return nil, err
	}
	var out []Reading
	cur := graph.Data.Connection.GlucoseMeasurement
	curT, curOK := parseFactory(cur.FactoryTimestamp)
	if curOK && cur.ValueInMgPerDl > 0 {
		out = append(out, Reading{curT, cur.ValueInMgPerDl, trend(cur.TrendArrow)})
	}
	for _, m := range graph.Data.GraphData {
		t, ok := parseFactory(m.FactoryTimestamp)
		// History has no trend; skip a point that duplicates the current
		// reading so it doesn't overwrite the trend.
		if !ok || m.ValueInMgPerDl <= 0 || (curOK && t.Equal(curT)) {
			continue
		}
		out = append(out, Reading{Time: t, Value: m.ValueInMgPerDl})
	}
	return out, nil
}

// ToEntries converts readings to Nightscout entry documents.
func ToEntries(rs []Reading) []store.Doc {
	docs := make([]store.Doc, 0, len(rs))
	for _, r := range rs {
		d := store.Doc{
			"type":       "sgv",
			"sgv":        r.Value,
			"date":       r.Time.UnixMilli(),
			"dateString": r.Time.Format("2006-01-02T15:04:05.000Z"),
			"device":     "librelinkup",
		}
		if r.Direction != "" {
			d["direction"] = r.Direction
		}
		docs = append(docs, d)
	}
	return docs
}

// Poll fetches readings every interval and passes them to insert, backing
// off on errors.
func Poll(ctx context.Context, c *Client, interval time.Duration, insert func(context.Context, []store.Doc) error, log *slog.Logger) {
	wait := time.Duration(0)
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		rs, err := c.Latest(ctx)
		switch {
		case err != nil:
			log.Warn("librelinkup fetch failed", "err", err)
			wait = min(max(wait*2, interval), 30*time.Minute)
		default:
			if len(rs) > 0 {
				if err := insert(ctx, ToEntries(rs)); err != nil {
					log.Error("librelinkup insert failed", "err", err)
				}
			}
			wait = interval
		}
	}
}
