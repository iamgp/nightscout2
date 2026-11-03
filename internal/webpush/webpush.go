// Package webpush sends Web Push notifications (RFC 8030) with message
// encryption (RFC 8291, aes128gcm) and VAPID authentication (RFC 8292),
// using only the standard library.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var b64 = base64.RawURLEncoding

// decodeB64 accepts base64url with or without padding (browsers vary).
func decodeB64(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("invalid base64")
}

// Keys is a VAPID key pair identifying this server to push services.
type Keys struct {
	Private *ecdsa.PrivateKey
}

// GenerateKeys creates a new VAPID key pair.
func GenerateKeys() (*Keys, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Keys{Private: k}, nil
}

// PublicKey returns the uncompressed public key, base64url-encoded: the
// applicationServerKey browsers need to subscribe.
func (k *Keys) PublicKey() string {
	pub, err := k.Private.PublicKey.ECDH()
	if err != nil {
		panic(err)
	}
	return b64.EncodeToString(pub.Bytes())
}

// Marshal serialises the private key for storage.
func (k *Keys) Marshal() string {
	priv, err := k.Private.ECDH()
	if err != nil {
		panic(err)
	}
	return b64.EncodeToString(priv.Bytes())
}

// ParseKeys restores keys produced by Marshal.
func ParseKeys(s string) (*Keys, error) {
	d, err := decodeB64(s)
	if err != nil || len(d) != 32 {
		return nil, errors.New("invalid VAPID private key")
	}
	priv, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return nil, err
	}
	pub := priv.PublicKey().Bytes() // 0x04 || X || Y
	k := &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{
			Curve: elliptic.P256(),
			X:     new(big.Int).SetBytes(pub[1:33]),
			Y:     new(big.Int).SetBytes(pub[33:65]),
		},
		D: new(big.Int).SetBytes(d),
	}
	return &Keys{Private: k}, nil
}

// Subscription is what the browser's PushManager.subscribe() returns.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	P256dh   string `json:"p256dh"`
	Auth     string `json:"auth"`
}

// Urgency hints how quickly the push service should deliver (RFC 8030).
type Urgency string

// Urgency levels.
const (
	UrgencyNormal Urgency = "normal"
	UrgencyHigh   Urgency = "high"
)

// ErrGone means the subscription no longer exists and should be deleted.
var ErrGone = errors.New("push subscription is gone")

// Client sends push messages.
type Client struct {
	Keys    *Keys
	Subject string // contact URL or mailto: for the push service operator
	HTTP    *http.Client
}

// Send encrypts payload for the subscription and delivers it.
func (c *Client) Send(ctx context.Context, sub Subscription, payload []byte, ttl time.Duration, urgency Urgency) error {
	u, err := url.Parse(sub.Endpoint)
	if err != nil || u.Host == "" {
		return errors.New("invalid push endpoint")
	}
	body, err := Encrypt(sub, payload)
	if err != nil {
		return err
	}
	jwt, err := c.vapidJWT(u.Scheme+"://"+u.Host, time.Now().Add(12*time.Hour))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("TTL", strconv.Itoa(int(ttl.Seconds())))
	if urgency != "" {
		req.Header.Set("Urgency", string(urgency))
	}
	req.Header.Set("Authorization", "vapid t="+jwt+", k="+c.Keys.PublicKey())
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 300:
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("push service returned %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

func (c *Client) vapidJWT(aud string, exp time.Time) (string, error) {
	header := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": aud, "exp": exp.Unix(), "sub": c.Subject})
	signing := header + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, c.Keys.Private, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signing + "." + b64.EncodeToString(sig), nil
}

// Encrypt produces an aes128gcm-encoded message body for the subscription
// (RFC 8291 section 3 and RFC 8188).
func Encrypt(sub Subscription, payload []byte) ([]byte, error) {
	uaPubBytes, err := decodeB64(sub.P256dh)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	authSecret, err := decodeB64(sub.Auth)
	if err != nil || len(authSecret) < 16 {
		return nil, errors.New("invalid auth secret")
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPubBytes)
	if err != nil {
		return nil, fmt.Errorf("p256dh: %w", err)
	}
	asPriv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	return encryptWith(asPriv, uaPub, authSecret, salt, payload)
}

func deriveKeys(shared, authSecret, uaPub, asPub, salt []byte) (cek, nonce []byte, err error) {
	keyInfo := append(append([]byte("WebPush: info\x00"), uaPub...), asPub...)
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, nil, err
	}
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, nil, err
	}
	if cek, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16); err != nil {
		return nil, nil, err
	}
	nonce, err = hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	return cek, nonce, err
}

const recordSize = 4096

func encryptWith(asPriv *ecdh.PrivateKey, uaPub *ecdh.PublicKey, authSecret, salt, payload []byte) ([]byte, error) {
	if len(payload) > recordSize-16-1-86 {
		return nil, errors.New("payload too large for a single record")
	}
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := asPriv.PublicKey().Bytes()
	cek, nonce, err := deriveKeys(shared, authSecret, uaPub.Bytes(), asPub, salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain := append(append([]byte{}, payload...), 0x02) // last-record delimiter, no padding
	ct := gcm.Seal(nil, nonce, plain, nil)

	header := make([]byte, 0, 16+4+1+len(asPub))
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, recordSize)
	header = append(header, byte(len(asPub)))
	header = append(header, asPub...)
	return append(header, ct...), nil
}

// Decrypt reverses Encrypt given the subscriber's private key. Tideline only
// needs it for tests, but it documents the format.
func Decrypt(uaPriv *ecdh.PrivateKey, authSecret, body []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, errors.New("body too short")
	}
	salt := body[:16]
	idlen := int(body[20])
	if len(body) < 21+idlen {
		return nil, errors.New("body too short")
	}
	asPub, err := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	if err != nil {
		return nil, err
	}
	shared, err := uaPriv.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	cek, nonce, err := deriveKeys(shared, authSecret, uaPriv.PublicKey().Bytes(), asPub.Bytes(), salt)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	i := bytes.LastIndexByte(plain, 0x02)
	if i < 0 {
		return nil, errors.New("missing record delimiter")
	}
	return plain[:i], nil
}
