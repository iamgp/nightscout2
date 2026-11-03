package webpush

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := decodeB64(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// RFC 8291 Appendix A.
func TestRFC8291Vector(t *testing.T) {
	asPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPriv, err := ecdh.P256().NewPrivateKey(mustB64(t, "q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b64.EncodeToString(uaPriv.PublicKey().Bytes()); got != "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4" {
		t.Fatalf("ua public key mismatch: %s", got)
	}
	auth := mustB64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := mustB64(t, "DGv6ra1nlYgDCS1FRnbzlw")
	body, err := encryptWith(asPriv, uaPriv.PublicKey(), auth, salt, []byte("When I grow up, I want to be a watermelon"))
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if got := b64.EncodeToString(body); got != want {
		t.Fatalf("ciphertext mismatch\n got %s\nwant %s", got, want)
	}
	plain, err := Decrypt(uaPriv, auth, body)
	if err != nil || string(plain) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("decrypt: %q %v", plain, err)
	}
}

func TestKeysRoundTrip(t *testing.T) {
	k, err := GenerateKeys()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := ParseKeys(k.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if k.PublicKey() != k2.PublicKey() || len(mustB64(t, k.PublicKey())) != 65 {
		t.Fatal("public key changed after round trip")
	}
	if _, err := ParseKeys("bogus"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSendDeliversDecryptablePayloadWithValidVAPID(t *testing.T) {
	uaPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	keys, _ := GenerateKeys()

	var got []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		p, err := Decrypt(uaPriv, auth, body)
		if err != nil {
			t.Errorf("decrypt: %v", err)
		}
		got = p
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c := &Client{Keys: keys, Subject: "https://example.org/tideline"}
	sub := Subscription{Endpoint: srv.URL + "/push/abc", P256dh: b64.EncodeToString(uaPriv.PublicKey().Bytes()), Auth: b64.EncodeToString(auth)}
	if err := c.Send(context.Background(), sub, []byte(`{"title":"Low"}`), time.Hour, UrgencyHigh); err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"title":"Low"}` {
		t.Fatalf("payload: %q", got)
	}
	if hdr.Get("Content-Encoding") != "aes128gcm" || hdr.Get("Urgency") != "high" || hdr.Get("TTL") != "3600" {
		t.Fatalf("headers: %v", hdr)
	}

	// Verify the VAPID header as a push service would.
	a := hdr.Get("Authorization")
	if !strings.HasPrefix(a, "vapid t=") || !strings.Contains(a, ", k="+keys.PublicKey()) {
		t.Fatalf("authorization: %s", a)
	}
	jwt := strings.TrimPrefix(strings.Split(a, ", k=")[0], "vapid t=")
	parts := strings.Split(jwt, ".")
	var claims map[string]any
	json.Unmarshal(mustB64(t, parts[1]), &claims)
	if claims["aud"] != srv.URL || claims["sub"] != "https://example.org/tideline" {
		t.Fatalf("claims: %v", claims)
	}
	sig := mustB64(t, parts[2])
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&keys.Private.PublicKey, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("VAPID signature does not verify")
	}
}

func TestSendReportsGoneSubscriptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer srv.Close()
	uaPriv, _ := ecdh.P256().GenerateKey(rand.Reader)
	keys, _ := GenerateKeys()
	c := &Client{Keys: keys, Subject: "mailto:a@example.org"}
	sub := Subscription{Endpoint: srv.URL, P256dh: b64.EncodeToString(uaPriv.PublicKey().Bytes()), Auth: b64.EncodeToString(make([]byte, 16))}
	if err := c.Send(context.Background(), sub, []byte("x"), time.Minute, UrgencyNormal); err != ErrGone {
		t.Fatalf("got %v, want ErrGone", err)
	}
}
