// Package signature signs and verifies Webhook Redrive's v1 wire format.
// Verify the original request bytes before parsing or processing the payload.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const Prefix = "v1="

var (
	ErrInvalidSignature = errors.New("invalid signature")
	ErrStaleTimestamp   = errors.New("timestamp outside tolerance")
)

// Sign authenticates timestamp + "." + body. The body bytes must be the exact
// bytes sent over HTTP.
func Sign(secret []byte, timestamp time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(strconv.FormatInt(timestamp.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return Prefix + hex.EncodeToString(mac.Sum(nil))
}

func Verify(secret []byte, timestamp string, body []byte, presented string, now time.Time, tolerance time.Duration) error {
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || tolerance < 0 || strconv.FormatInt(seconds, 10) != timestamp {
		return ErrStaleTimestamp
	}
	signedAt := time.Unix(seconds, 0)
	age := now.Sub(signedAt)
	if age < -tolerance || age > tolerance {
		return ErrStaleTimestamp
	}
	if len(presented) != len(Prefix)+sha256.Size*2 || !strings.HasPrefix(presented, Prefix) {
		return ErrInvalidSignature
	}
	received, err := hex.DecodeString(strings.TrimPrefix(presented, Prefix))
	if err != nil {
		return ErrInvalidSignature
	}
	expected, _ := hex.DecodeString(strings.TrimPrefix(Sign(secret, signedAt, body), Prefix))
	if !hmac.Equal(received, expected) {
		return ErrInvalidSignature
	}
	return nil
}

// VerifyRequest rejects ambiguous duplicate authentication headers. It does not
// read the body, authenticate other headers, or deduplicate business processing.
func VerifyRequest(secret []byte, request *http.Request, body []byte, now time.Time, tolerance time.Duration) error {
	if len(request.Header.Values("X-Webhook-Timestamp")) != 1 || len(request.Header.Values("X-Webhook-Signature")) != 1 {
		return ErrInvalidSignature
	}
	return Verify(secret, request.Header.Get("X-Webhook-Timestamp"), body, request.Header.Get("X-Webhook-Signature"), now, tolerance)
}
