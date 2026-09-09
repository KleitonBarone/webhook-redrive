package signature

import (
	"errors"
	"testing"
	"time"
)

func TestVerifyExactBytesAndTimestampTolerance(t *testing.T) {
	t.Parallel()
	secret := []byte("synthetic-secret-32-bytes-long")
	signedAt := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	body := []byte("{\n  \"amount\": 42\n}\n")
	signature := Sign(secret, signedAt, body)

	if err := Verify(secret, "1787832000", body, signature, signedAt.Add(5*time.Minute), 5*time.Minute); err != nil {
		t.Fatalf("verify at tolerance boundary: %v", err)
	}
	if err := Verify(secret, "1787832000", []byte(`{"amount":42}`), signature, signedAt, 5*time.Minute); !errors.Is(err, ErrInvalidSignature) {
		t.Fatalf("normalized body should fail verification, got %v", err)
	}
	if err := Verify(secret, "1787832000", body, signature, signedAt.Add(5*time.Minute+time.Second), 5*time.Minute); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("stale timestamp should fail verification, got %v", err)
	}
	if err := Verify(secret, "1787832000", body, signature, signedAt.Add(-5*time.Minute-time.Second), 5*time.Minute); !errors.Is(err, ErrStaleTimestamp) {
		t.Fatalf("future timestamp should fail verification, got %v", err)
	}
}
