package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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
	if err != nil {
		return ErrStaleTimestamp
	}
	signedAt := time.Unix(seconds, 0)
	age := now.Sub(signedAt)
	if age < -tolerance || age > tolerance {
		return ErrStaleTimestamp
	}
	if !strings.HasPrefix(presented, Prefix) {
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
