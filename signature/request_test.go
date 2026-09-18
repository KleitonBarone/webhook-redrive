package signature_test

import (
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KleitonBarone/webhook-redrive/signature"
)

func TestPublicWireVectorAndMalformedHeaders(t *testing.T) {
	secret := []byte("synthetic-secret-32-bytes-long")
	body := []byte(`{"amount":42}`)
	now := time.Unix(1787832000, 0)
	const expected = "v1=ad8d57359db70fdd976ce33f2b7ab6e83e9ce9c1e0857f6db970fe81a3f9ea3b"
	if signature.Sign(secret, now, body) != expected {
		t.Fatal("wire format changed")
	}
	for _, delta := range []time.Duration{-5 * time.Minute, 0, 5 * time.Minute} {
		if err := signature.Verify(secret, "1787832000", body, expected, now.Add(delta), 5*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	for _, stamp := range []string{"", "+1787832000", "01787832000", "1787832000.0", "9223372036854775808"} {
		if err := signature.Verify(secret, stamp, body, expected, now, 5*time.Minute); !errors.Is(err, signature.ErrStaleTimestamp) {
			t.Fatal("invalid timestamp accepted")
		}
	}
	if signature.Verify(secret, "1787832000", body, expected, now, -time.Second) == nil {
		t.Fatal("negative tolerance accepted")
	}
	for _, presented := range []string{"", "v2=" + expected[3:], expected + "a", "v1=xyz"} {
		if signature.Verify(secret, "1787832000", body, presented, now, time.Minute) == nil {
			t.Fatal("malformed signature accepted")
		}
	}
	if signature.Verify([]byte("wrong-synthetic-secret"), "1787832000", body, expected, now, time.Minute) == nil {
		t.Fatal("wrong key accepted")
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Webhook-Timestamp", "1787832000")
	req.Header.Set("X-Webhook-Signature", expected)
	if err := signature.VerifyRequest(secret, req, body, now, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"X-Webhook-Timestamp", "X-Webhook-Signature"} {
		copy := req.Clone(req.Context())
		copy.Header.Add(header, copy.Header.Get(header))
		if signature.VerifyRequest(secret, copy, body, now, time.Minute) == nil {
			t.Fatal("duplicate header accepted")
		}
	}
}

func TestRotationKeyRing(t *testing.T) {
	old, newKey := []byte("synthetic-old-signing-secret"), []byte("synthetic-new-signing-secret")
	body := []byte(`{"id":"synthetic"}`)
	now := time.Unix(1787832000, 0)
	for _, key := range [][]byte{old, newKey} {
		req := httptest.NewRequest("POST", "/", nil)
		req.Header.Set("X-Webhook-Timestamp", "1787832000")
		req.Header.Set("X-Webhook-Signature", signature.Sign(key, now, body))
		if err := signature.VerifyRequestKeys([][]byte{old, newKey}, req, body, now, time.Minute); err != nil {
			t.Fatal(err)
		}
		if signature.VerifyRequestKeys([][]byte{old, newKey}, req, body, now.Add(time.Minute+time.Second), time.Minute) == nil {
			t.Fatal("stale overlap signature accepted")
		}
		req.Header.Add("X-Webhook-Signature", req.Header.Get("X-Webhook-Signature"))
		if signature.VerifyRequestKeys([][]byte{old, newKey}, req, body, now, time.Minute) == nil {
			t.Fatal("duplicate overlap header accepted")
		}
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("X-Webhook-Timestamp", "1787832000")
	req.Header.Set("X-Webhook-Signature", signature.Sign(old, now, body))
	if signature.VerifyRequestKeys([][]byte{newKey}, req, body, now, time.Minute) == nil {
		t.Fatal("retired key accepted")
	}
	for _, keys := range [][][]byte{nil, {old, []byte("short")}, {old, newKey, old}} {
		if signature.VerifyRequestKeys(keys, req, body, now, time.Minute) == nil {
			t.Fatal("invalid key ring accepted")
		}
	}
}
