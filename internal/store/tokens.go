package store

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// Token is an access token. The secret itself is never stored, only its hash.
type Token struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	Prefix     string `json:"prefix"` // first characters, to help people recognise it
	CreatedAt  int64  `json:"createdAt"`
	ExpiresAt  *int64 `json:"expiresAt,omitempty"`
	LastUsedAt *int64 `json:"lastUsedAt,omitempty"`
	RevokedAt  *int64 `json:"revokedAt,omitempty"`
}

// Active reports whether the token can currently be used.
func (t Token) Active(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	return t.ExpiresAt == nil || now.UnixMilli() < *t.ExpiresAt
}

// HashToken returns the stored hash for a plaintext token.
func HashToken(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}

// SHA1Hex returns the hex SHA-1 of a token: what Nightscout uploaders send in
// the api-secret header when the token is typed into their "API secret" field.
func SHA1Hex(plain string) string {
	h := sha1.Sum([]byte(plain))
	return hex.EncodeToString(h[:])
}

func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
				b.WriteByte('-')
			}
		}
		if b.Len() >= 20 {
			break
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "token"
	}
	return s
}

// CreateToken issues a new token and returns its plaintext once. The format
// "<name>-<16 hex>" matches Nightscout's access tokens, so uploaders that
// validate the token shape accept it.
func (s *Store) CreateToken(ctx context.Context, name, role string, expiresAt *time.Time) (string, Token, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", Token{}, err
	}
	plain := slug(name) + "-" + hex.EncodeToString(b[:])
	t := Token{
		ID:        NewID(),
		Name:      name,
		Role:      role,
		Prefix:    plain[:min(len(plain), len(slug(name))+5)],
		CreatedAt: time.Now().UnixMilli(),
	}
	var exp any
	if expiresAt != nil {
		ms := expiresAt.UnixMilli()
		t.ExpiresAt = &ms
		exp = ms
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tokens (id, name, role, hash, sha1, prefix, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, t.Name, t.Role, HashToken(plain), SHA1Hex(plain), t.Prefix, t.CreatedAt, exp)
	if err != nil {
		return "", Token{}, err
	}
	return plain, t, nil
}

const tokenCols = `id, name, role, prefix, created_at, expires_at, last_used_at, revoked_at`

func scanToken(sc interface{ Scan(...any) error }) (Token, error) {
	var t Token
	var exp, used, rev sql.NullInt64
	if err := sc.Scan(&t.ID, &t.Name, &t.Role, &t.Prefix, &t.CreatedAt, &exp, &used, &rev); err != nil {
		return t, err
	}
	if exp.Valid {
		t.ExpiresAt = &exp.Int64
	}
	if used.Valid {
		t.LastUsedAt = &used.Int64
	}
	if rev.Valid {
		t.RevokedAt = &rev.Int64
	}
	return t, nil
}

// LookupToken finds a token by its plaintext value.
func (s *Store) LookupToken(ctx context.Context, plain string) (Token, bool, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE hash = ?`, HashToken(plain)))
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

// LookupTokenByID finds a token by its ID.
func (s *Store) LookupTokenByID(ctx context.Context, id string) (Token, bool, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

// LookupTokenSHA1 finds a token by the hex SHA-1 of its plaintext.
func (s *Store) LookupTokenSHA1(ctx context.Context, sha1hex string) (Token, bool, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM tokens WHERE sha1 = ?`, strings.ToLower(sha1hex)))
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	return t, err == nil, err
}

// ListTokens returns all tokens, newest first.
func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes a token. Returns false if no active token had that ID.
func (s *Store) RevokeToken(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
		time.Now().UnixMilli(), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// TouchToken records that a token was used.
func (s *Store) TouchToken(ctx context.Context, id string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, at.UnixMilli(), id)
	return err
}
