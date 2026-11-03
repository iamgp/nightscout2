package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/iamgp/tideline/internal/settings"
)

// HTTPSender delivers notifications over HTTP to ntfy, Pushover or a webhook.
type HTTPSender struct {
	Client      *http.Client
	Units       func() string // display units for messages
	PushoverURL string        // override for tests
	SiteName    func() string
	BaseURL     string // public URL of this Tideline, linked from notifications
}

// FormatGlucose renders a mg/dL value in the given units.
func FormatGlucose(mgdl float64, units string) string {
	if units == settings.UnitsMmol {
		return fmt.Sprintf("%.1f mmol/L", mgdl/18)
	}
	return fmt.Sprintf("%.0f mg/dL", mgdl)
}

func (h *HTTPSender) text(a Alert, resolved bool) (title, body string) {
	units := settings.UnitsMgdl
	if h.Units != nil {
		units = h.Units()
	}
	site := "Tideline"
	if h.SiteName != nil && h.SiteName() != "" {
		site = h.SiteName()
	}
	title = fmt.Sprintf("%s: %s", site, a.Title)
	body = a.Message
	if a.Value > 0 {
		v := FormatGlucose(a.Value, units)
		if body == "" {
			body = v
		} else {
			body = v + " · " + body
		}
	}
	if resolved {
		title = fmt.Sprintf("%s: resolved – %s", site, a.Title)
		body = "This alert has cleared."
	}
	return title, body
}

// Send implements Sender.
func (h *HTTPSender) Send(ctx context.Context, n settings.Notifier, a Alert, resolved bool) error {
	client := h.Client
	if client == nil {
		client = http.DefaultClient
	}
	title, body := h.text(a, resolved)
	var req *http.Request
	var err error
	switch n.Type {
	case "ntfy":
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, n.URL, strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Title", title)
		prio, tags := "4", "warning"
		if a.Level == LevelUrgent {
			prio, tags = "5", "rotating_light"
		}
		if resolved {
			prio, tags = "3", "white_check_mark"
		}
		req.Header.Set("Priority", prio)
		req.Header.Set("Tags", tags)
		if h.BaseURL != "" {
			req.Header.Set("Click", h.BaseURL)
		}
		if n.Token != "" {
			req.Header.Set("Authorization", "Bearer "+n.Token)
		}
	case "pushover":
		form := url.Values{
			"token":   {n.Token},
			"user":    {n.User},
			"title":   {title},
			"message": {body},
		}
		switch {
		case resolved:
			form.Set("priority", "0")
		case a.Level == LevelUrgent:
			// Emergency priority: repeats until acknowledged in the Pushover app.
			form.Set("priority", "2")
			form.Set("retry", "60")
			form.Set("expire", "1800")
		default:
			form.Set("priority", "1")
		}
		if h.BaseURL != "" {
			form.Set("url", h.BaseURL)
		}
		u := h.PushoverURL
		if u == "" {
			u = "https://api.pushover.net/1/messages.json"
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case "webhook":
		payload, _ := json.Marshal(map[string]any{
			"title":    title,
			"message":  body,
			"alert":    a,
			"resolved": resolved,
		})
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if n.Token != "" {
			req.Header.Set("Authorization", "Bearer "+n.Token)
		}
	default:
		return fmt.Errorf("unknown notifier type %q", n.Type)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s returned %s: %s", n.Type, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}
