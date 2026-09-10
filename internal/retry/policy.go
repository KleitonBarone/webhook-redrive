package retry

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const MaxRetryAfter = 24 * time.Hour

// Status explicitly lists responses that can recover without changing the event.
func Status(status int) bool {
	switch status {
	case 408, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

func Transport(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	var unknown x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var hostname x509.HostnameError
	if errors.As(err, &unknown) || errors.As(err, &invalid) || errors.As(err, &hostname) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTimeout || dns.IsTemporary
	}
	var network *net.OpError
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &network)
}

// Delay uses equal jitter: half the capped exponential delay plus a random
// fraction of the other half. Supplying the fraction makes tests deterministic.
func Delay(cycleAttempt int, fraction float64) time.Duration {
	ceiling := time.Second
	for n := 1; n < cycleAttempt && ceiling < time.Minute; n++ {
		ceiling *= 2
	}
	ceiling = min(ceiling, time.Minute)
	fraction = max(0, min(1, fraction))
	return ceiling/2 + time.Duration(float64(ceiling/2)*fraction)
}

// After accepts delta seconds or an HTTP date, bounded to 24 hours.
func After(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0
		}
		if seconds >= int64(MaxRetryAfter/time.Second) {
			return MaxRetryAfter
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil {
		return max(0, min(date.Sub(now), MaxRetryAfter))
	}
	return 0
}
