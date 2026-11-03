package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// JWTLifetime is how long an issued JWT stays valid, matching Nightscout.
const JWTLifetime = 8 * time.Hour

var b64 = base64.RawURLEncoding

type jwtClaims struct {
	Sub  string `json:"sub"` // token ID
	Name string `json:"name"`
	Iat  int64  `json:"iat"`
	Exp  int64  `json:"exp"`
}

func (a *Authenticator) jwtKey() []byte {
	m := hmac.New(sha256.New, []byte(a.secret))
	m.Write([]byte("tideline/jwt/v1"))
	return m.Sum(nil)
}

// IssueJWT exchanges an access token for a short-lived JWT, as Nightscout's
// /api/v2/authorization/request/{token} does for API v3 clients.
//
// The JWT names the token by ID, not by value, and every request re-checks
// that the token still exists and is active, so revoking a token cuts off
// its JWTs immediately.
func (a *Authenticator) IssueJWT(ctx context.Context, accessToken string) (jwt string, p *Principal, iat, exp time.Time, err error) {
	t, ok, err := a.tokens.LookupToken(ctx, accessToken)
	if err != nil {
		return "", nil, iat, exp, err
	}
	now := time.Now()
	if !ok || !t.Active(now) {
		return "", nil, iat, exp, ErrBadCredentials{}
	}
	iat, exp = now, now.Add(JWTLifetime)
	if t.ExpiresAt != nil && time.UnixMilli(*t.ExpiresAt).Before(exp) {
		exp = time.UnixMilli(*t.ExpiresAt)
	}
	header := b64.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	body, _ := json.Marshal(jwtClaims{Sub: t.ID, Name: t.Name, Iat: iat.Unix(), Exp: exp.Unix()})
	signing := header + "." + b64.EncodeToString(body)
	m := hmac.New(sha256.New, a.jwtKey())
	m.Write([]byte(signing))
	a.touch(ctx, t.ID, now)
	return signing + "." + b64.EncodeToString(m.Sum(nil)), &Principal{Name: t.Name, Role: Role(t.Role), TokenID: t.ID}, iat, exp, nil
}

func looksLikeJWT(s string) bool {
	return strings.HasPrefix(s, "eyJ") && strings.Count(s, ".") == 2
}

var errBadJWT = errors.New("invalid or expired JWT")

func (a *Authenticator) verifyJWT(ctx context.Context, jwt string) (*Principal, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, errBadJWT
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, errBadJWT
	}
	m := hmac.New(sha256.New, a.jwtKey())
	m.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, m.Sum(nil)) {
		return nil, errBadJWT
	}
	hdr, err := b64.DecodeString(parts[0])
	if err != nil || !strings.Contains(string(hdr), `"HS256"`) {
		return nil, errBadJWT
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, errBadJWT
	}
	var c jwtClaims
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, errBadJWT
	}
	now := time.Now()
	if now.Unix() >= c.Exp {
		return nil, errBadJWT
	}
	t, ok, err := a.tokens.LookupTokenByID(ctx, c.Sub)
	if err != nil {
		return nil, err
	}
	if !ok || !t.Active(now) {
		return nil, errBadJWT
	}
	a.touch(ctx, t.ID, now)
	return &Principal{Name: t.Name, Role: Role(t.Role), TokenID: t.ID}, nil
}
