package store

import (
	"context"
	"database/sql"
	"time"
)

// PushSubscription is a browser or phone registered for Web Push alerts.
// Any signed-in person can register their own devices; subscriptions belong
// to the token that created them and stop receiving alerts when that token
// is revoked or expires.
type PushSubscription struct {
	ID                   string `json:"id"`
	Endpoint             string `json:"-"`
	P256dh               string `json:"-"`
	Auth                 string `json:"-"`
	TokenID              string `json:"-"`
	Owner                string `json:"owner"`
	Name                 string `json:"name"`
	MinLevel             string `json:"minLevel"`
	EscalateAfterMinutes int    `json:"escalateAfterMinutes"`
	CreatedAt            int64  `json:"createdAt"`
	LastSuccessAt        *int64 `json:"lastSuccessAt,omitempty"`
	Failures             int    `json:"failures"`
	Mine                 bool   `json:"mine"`
}

const pushCols = `p.id, p.endpoint, p.p256dh, p.auth, COALESCE(p.token_id, ''), p.owner, p.name,
	p.min_level, p.escalate_after, p.created_at, p.last_success_at, p.failures`

func scanPush(rows *sql.Rows) ([]PushSubscription, error) {
	defer rows.Close()
	out := []PushSubscription{}
	for rows.Next() {
		var p PushSubscription
		var last sql.NullInt64
		if err := rows.Scan(&p.ID, &p.Endpoint, &p.P256dh, &p.Auth, &p.TokenID, &p.Owner, &p.Name,
			&p.MinLevel, &p.EscalateAfterMinutes, &p.CreatedAt, &last, &p.Failures); err != nil {
			return nil, err
		}
		if last.Valid {
			p.LastSuccessAt = &last.Int64
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SavePushSubscription registers a device, replacing any existing
// registration for the same endpoint.
func (s *Store) SavePushSubscription(ctx context.Context, p PushSubscription) (PushSubscription, error) {
	if p.ID == "" {
		p.ID = NewID()
	}
	p.CreatedAt = time.Now().UnixMilli()
	var tok any
	if p.TokenID != "" {
		tok = p.TokenID
	}
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO push_subscriptions (id, endpoint, p256dh, auth, token_id, owner, name, min_level, escalate_after, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(endpoint) DO UPDATE SET p256dh = excluded.p256dh, auth = excluded.auth,
			token_id = excluded.token_id, owner = excluded.owner, name = excluded.name,
			min_level = excluded.min_level, escalate_after = excluded.escalate_after, failures = 0
		RETURNING id, created_at`,
		p.ID, p.Endpoint, p.P256dh, p.Auth, tok, p.Owner, p.Name, p.MinLevel, p.EscalateAfterMinutes, p.CreatedAt).
		Scan(&p.ID, &p.CreatedAt)
	return p, err
}

// ListPushSubscriptions returns every registered device.
func (s *Store) ListPushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pushCols+` FROM push_subscriptions p ORDER BY p.created_at DESC`)
	if err != nil {
		return nil, err
	}
	return scanPush(rows)
}

// ActivePushSubscriptions returns devices whose owning token is still valid.
// Devices registered with the API secret have no token and are always active.
func (s *Store) ActivePushSubscriptions(ctx context.Context) ([]PushSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pushCols+` FROM push_subscriptions p
		LEFT JOIN tokens t ON t.id = p.token_id
		WHERE p.token_id IS NULL OR (t.id IS NOT NULL AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at > ?))`,
		time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	return scanPush(rows)
}

// GetPushSubscription returns one device by ID.
func (s *Store) GetPushSubscription(ctx context.Context, id string) (PushSubscription, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+pushCols+` FROM push_subscriptions p WHERE p.id = ?`, id)
	if err != nil {
		return PushSubscription{}, false, err
	}
	ps, err := scanPush(rows)
	if err != nil || len(ps) == 0 {
		return PushSubscription{}, false, err
	}
	return ps[0], true, nil
}

// DeletePushSubscription removes a device by ID.
func (s *Store) DeletePushSubscription(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeletePushEndpoint removes a device by endpoint, e.g. when the push
// service reports it gone.
func (s *Store) DeletePushEndpoint(ctx context.Context, endpoint string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM push_subscriptions WHERE endpoint = ?`, endpoint)
	return err
}

// RecordPushResult tracks delivery health per device.
func (s *Store) RecordPushResult(ctx context.Context, endpoint string, ok bool) error {
	if ok {
		_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET last_success_at = ?, failures = 0 WHERE endpoint = ?`,
			time.Now().UnixMilli(), endpoint)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE push_subscriptions SET failures = failures + 1 WHERE endpoint = ?`, endpoint)
	return err
}
