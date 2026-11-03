// Package auth resolves who is making a request.
//
// Tideline accepts the same credentials as Nightscout so existing uploaders
// keep working:
//
//   - the API secret, sent as the SHA-1 hex digest in an "api-secret" header
//     (what xDrip+, Loop and Trio do) or as plaintext;
//   - access tokens, sent as ?token=…, "Authorization: Bearer …", or typed
//     into an uploader's "API secret" field (arriving as its SHA-1 in the
//     api-secret header), so each device can have its own revocable token.
//
// Unlike Nightscout, data is private by default: anonymous reads are refused
// unless public read access is switched on explicitly.
package auth

import (
	"context"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/iamgp/tideline/internal/store"
)

// Role is what a principal is allowed to do.
type Role string

// Roles, in increasing order of privilege.
const (
	RoleReader   Role = "reader"   // view data (followers, clinicians)
	RoleUploader Role = "uploader" // reader + write CGM, treatments, device status
	RoleAdmin    Role = "admin"    // everything, including settings and tokens
)

// ValidRole reports whether r is a known role.
func ValidRole(r string) bool {
	switch Role(r) {
	case RoleReader, RoleUploader, RoleAdmin:
		return true
	}
	return false
}

// Principal is an authenticated caller.
type Principal struct {
	Name    string `json:"name"`
	Role    Role   `json:"role"`
	TokenID string `json:"tokenId,omitempty"`
}

// CanRead reports whether the principal may read data.
func (p *Principal) CanRead() bool { return p != nil }

// CanWrite reports whether the principal may write care data.
func (p *Principal) CanWrite() bool {
	return p != nil && (p.Role == RoleUploader || p.Role == RoleAdmin)
}

// IsAdmin reports whether the principal may change settings and tokens.
func (p *Principal) IsAdmin() bool { return p != nil && p.Role == RoleAdmin }

// TokenStore is the subset of the store auth needs.
type TokenStore interface {
	LookupToken(ctx context.Context, plain string) (store.Token, bool, error)
	LookupTokenSHA1(ctx context.Context, sha1hex string) (store.Token, bool, error)
	LookupTokenByID(ctx context.Context, id string) (store.Token, bool, error)
	TouchToken(ctx context.Context, id string, at time.Time) error
}

// Authenticator resolves request credentials.
type Authenticator struct {
	secret     string
	secretSHA1 string
	tokens     TokenStore
	publicRead bool

	mu      sync.Mutex
	touched map[string]time.Time
}

// New creates an Authenticator.
func New(apiSecret string, tokens TokenStore, publicRead bool) *Authenticator {
	h := sha1.Sum([]byte(apiSecret))
	return &Authenticator{
		secret:     apiSecret,
		secretSHA1: hex.EncodeToString(h[:]),
		tokens:     tokens,
		publicRead: publicRead,
		touched:    map[string]time.Time{},
	}
}

// PublicRead reports whether anonymous visitors may read data.
func (a *Authenticator) PublicRead() bool { return a.publicRead }

// SecretSHA1 returns the hex SHA-1 of the API secret, as uploaders send it.
func (a *Authenticator) SecretSHA1() string { return a.secretSHA1 }

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (a *Authenticator) isSecret(v string) bool {
	return eq(v, a.secret) || eq(strings.ToLower(v), a.secretSHA1)
}

// ErrBadCredentials is returned when credentials were supplied but are invalid.
type ErrBadCredentials struct{}

func (ErrBadCredentials) Error() string { return "invalid credentials" }

// Resolve returns the principal for a request. It returns (nil, nil) when no
// credentials were given and public read is off, and ErrBadCredentials when
// credentials were given but don't match anything.
func (a *Authenticator) Resolve(r *http.Request) (*Principal, error) {
	if v := r.Header.Get("api-secret"); v != "" {
		if a.isSecret(v) {
			return &Principal{Name: "api-secret", Role: RoleAdmin}, nil
		}
		// Uploaders hash whatever is in their "API secret" field, so a
		// per-device token pasted there arrives as its SHA-1.
		if len(v) == 40 {
			t, ok, err := a.tokens.LookupTokenSHA1(r.Context(), v)
			if err != nil {
				return nil, err
			}
			if ok && t.Active(time.Now()) {
				a.touch(r.Context(), t.ID, time.Now())
				return &Principal{Name: t.Name, Role: Role(t.Role), TokenID: t.ID}, nil
			}
		}
		return a.resolveToken(r.Context(), v)
	}
	if v := r.URL.Query().Get("secret"); v != "" {
		if a.isSecret(v) {
			return &Principal{Name: "api-secret", Role: RoleAdmin}, nil
		}
		return nil, ErrBadCredentials{}
	}
	tok := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		tok = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	} else if v := r.URL.Query().Get("token"); v != "" {
		tok = v
	}
	if tok != "" {
		return a.resolveToken(r.Context(), tok)
	}
	if a.publicRead {
		return &Principal{Name: "public", Role: RoleReader}, nil
	}
	return nil, nil
}

func (a *Authenticator) resolveToken(ctx context.Context, tok string) (*Principal, error) {
	// The web UI lets admins sign in with the API secret, sent as a bearer token.
	if a.isSecret(tok) {
		return &Principal{Name: "api-secret", Role: RoleAdmin}, nil
	}
	if looksLikeJWT(tok) {
		p, err := a.verifyJWT(ctx, tok)
		if errors.Is(err, errBadJWT) {
			return nil, ErrBadCredentials{}
		}
		return p, err
	}
	t, ok, err := a.tokens.LookupToken(ctx, tok)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if !ok || !t.Active(now) {
		return nil, ErrBadCredentials{}
	}
	a.touch(ctx, t.ID, now)
	return &Principal{Name: t.Name, Role: Role(t.Role), TokenID: t.ID}, nil
}

// touch records token use at most once a minute per token.
func (a *Authenticator) touch(ctx context.Context, id string, now time.Time) {
	a.mu.Lock()
	last, ok := a.touched[id]
	if ok && now.Sub(last) < time.Minute {
		a.mu.Unlock()
		return
	}
	a.touched[id] = now
	a.mu.Unlock()
	_ = a.tokens.TouchToken(ctx, id, now)
}
