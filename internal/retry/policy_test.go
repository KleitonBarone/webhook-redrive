package retry

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestStatusClassification(t *testing.T) {
	for status := 100; status <= 599; status++ {
		want := status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504
		if Status(status) != want {
			t.Fatalf("status %d: retry=%v", status, Status(status))
		}
	}
}

func TestTransportClassification(t *testing.T) {
	for _, test := range []struct {
		err  error
		want bool
	}{
		{context.DeadlineExceeded, true}, {context.Canceled, false}, {io.EOF, true},
		{&net.OpError{Op: "dial", Err: errors.New("connection refused")}, true},
		{&net.DNSError{IsTimeout: true}, true}, {&net.DNSError{IsNotFound: true}, false},
		{x509.UnknownAuthorityError{}, false}, {x509.HostnameError{}, false},
		{x509.CertificateInvalidError{}, false}, {errors.New("unsupported protocol"), false},
	} {
		wrapped := &url.Error{Op: "Post", URL: "http://synthetic.test", Err: test.err}
		if got := Transport(wrapped); got != test.want {
			t.Errorf("%T: got %v want %v", test.err, got, test.want)
		}
	}
}

func TestBackoffAndRetryAfter(t *testing.T) {
	for _, test := range []struct {
		n        int
		fraction float64
		want     time.Duration
	}{
		{1, 0, 500 * time.Millisecond}, {1, 1, time.Second}, {2, 0, time.Second},
		{3, 0.5, 3 * time.Second}, {20, 1, time.Minute}, {100000, 0, 30 * time.Second},
	} {
		if got := Delay(test.n, test.fraction); got != test.want {
			t.Errorf("delay %+v: %s", test, got)
		}
	}
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"10", 10 * time.Second}, {"-1", 0}, {"nonsense", 0},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"9223372036854775807", MaxRetryAfter},
		{now.Add(48 * time.Hour).Format(http.TimeFormat), MaxRetryAfter},
	} {
		if got := After(test.value, now); got != test.want {
			t.Errorf("Retry-After %q: %s want %s", test.value, got, test.want)
		}
	}
}
