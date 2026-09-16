package destination

import (
	"context"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type lookupFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f lookupFunc) LookupNetIP(ctx context.Context, n, h string) ([]netip.Addr, error) {
	return f(ctx, n, h)
}

func TestAddressPolicy(t *testing.T) {
	restricted := rule{}
	private := rule{networks: prefixes("10.0.0.0/24", "fc00::/7", "127.0.0.1/32")}
	for _, tc := range []struct {
		ip                string
		public, exception bool
	}{
		{"8.8.8.8", true, true}, {"2606:4700:4700::1111", true, true},
		{"10.0.0.1", false, true}, {"10.0.1.1", false, false}, {"127.0.0.1", false, true},
		{"::ffff:127.0.0.1", false, true}, {"::1", false, false}, {"fd01::1", false, true},
		{"fd00:ec2::254", false, false}, {"169.254.169.254", false, false}, {"168.63.129.16", false, false},
		{"100.100.100.200", false, false}, {"0.0.0.0", false, false}, {"::", false, false},
		{"fe80::1", false, false}, {"ff02::1", false, false}, {"224.0.0.1", false, false},
		{"192.0.2.1", false, false}, {"2001:db8::1", false, false}, {"2002:7f00:1::", false, false},
		{"64:ff9b::7f00:1", false, false}, {"fe80::1%zone", false, false},
	} {
		ip := netip.MustParseAddr(tc.ip)
		if restricted.allows(ip) != tc.public || private.allows(ip) != tc.exception {
			t.Errorf("incorrect policy for %s", tc.ip)
		}
	}
}

func TestExactOriginsAndPolicyValidation(t *testing.T) {
	p, err := New([]Rule{{Origin: "https://hooks.example.test"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"https://hooks.example.test/path?value=1", "https://HOOKS.example.test:443/hook"} {
		if err := p.Validate(u); err != nil {
			t.Errorf("approved URL rejected: %s", u)
		}
	}
	for _, u := range []string{"http://hooks.example.test", "https://hooks.example.test:444", "https://hooks.example.test.evil.test",
		"https://user:secret@hooks.example.test", "https://hooks.example.test/#fragment", "https://hooks.example.test.",
		"https://hooks.example.test:", "https://hooks.example.test:0443", "file:///tmp/a", "//hooks.example.test", "https://[fe80::1%25eth0]/"} {
		if err := p.Validate(u); !errors.Is(err, ErrDenied) {
			t.Errorf("unapproved URL accepted: %s", u)
		}
	}
	for _, rules := range [][]Rule{
		{{Origin: "https://*.example.test"}}, {{Origin: "https://example.test/path"}},
		{{Origin: "https://example.test"}, {Origin: "https://example.test:443"}},
		{{Origin: "https://example.test", PrivateNetworks: []string{"0.0.0.0/0"}}},
		{{Origin: "https://example.test", PrivateNetworks: []string{"169.254.0.0/16"}}},
	} {
		if _, err := New(rules); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	var absent *Policy
	if absent.Validate("https://example.test") == nil {
		t.Fatal("nil policy allowed destination")
	}
}

func TestDialRejectsMixedAnswersAndPinsValidatedIP(t *testing.T) {
	for _, tc := range []struct {
		answers []netip.Addr
		denied  bool
	}{
		{[]netip.Addr{netip.MustParseAddr("8.8.8.8")}, false},
		{[]netip.Addr{netip.MustParseAddr("2606:4700:4700::1111")}, false},
		{[]netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("127.0.0.1")}, true},
		{[]netip.Addr{netip.MustParseAddr("::ffff:169.254.169.254")}, true},
	} {
		p, _ := New(nil)
		lookups, dials := 0, 0
		p.resolver = lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) { lookups++; return tc.answers, nil })
		dialFailure := errors.New("synthetic dial failure")
		p.dial = func(_ context.Context, _ string, address string) (net.Conn, error) {
			dials++
			if address != net.JoinHostPort(tc.answers[0].Unmap().String(), "443") {
				t.Error("dial did not use validated literal IP")
			}
			return nil, dialFailure
		}
		_, err := p.dialContext(context.WithValue(context.Background(), ruleKey{}, rule{}), "tcp", "hooks.test:443")
		if lookups != 1 || tc.denied && (dials != 0 || !errors.Is(err, ErrDenied)) || !tc.denied && (dials != 1 || !errors.Is(err, dialFailure)) {
			t.Fatalf("lookup=%d dial=%d err=%v", lookups, dials, err)
		}
	}
}

func TestTransportRechecksDNSAndDoesNotUseProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer receiver.Close()
	p, _ := New([]Rule{{Origin: "http://receiver.test"}})
	lookups, dials := 0, 0
	p.resolver = lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	})
	p.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		dials++
		if address != "8.8.8.8:80" {
			t.Error("unexpected dial")
		}
		return (&net.Dialer{}).DialContext(ctx, network, receiver.Listener.Addr().String())
	}
	client := &http.Client{Transport: p.Transport(), Timeout: time.Second}
	response, err := client.Get("http://receiver.test/hook")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	client.CloseIdleConnections()
	if _, err = client.Get("http://receiver.test/hook"); !errors.Is(err, ErrDenied) {
		t.Fatal("DNS change bypassed policy")
	}
	if lookups != 2 || dials != 1 {
		t.Fatalf("lookups %d dials %d", lookups, dials)
	}
}

func TestTransportPreservesTLSHostnameVerification(t *testing.T) {
	receiver := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer receiver.Close()
	name := receiver.Certificate().DNSNames[0]
	p, _ := New([]Rule{{Origin: "https://" + name}, {Origin: "https://wrong-name.test"}})
	p.resolver = lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	})
	p.dial = func(ctx context.Context, n, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, receiver.Listener.Addr().String())
	}
	tr := p.Transport().(*transport)
	roots := x509.NewCertPool()
	roots.AddCert(receiver.Certificate())
	tr.http.TLSClientConfig.RootCAs = roots
	client := &http.Client{Transport: tr, Timeout: time.Second}
	defer client.CloseIdleConnections()
	response, err := client.Get("https://" + name)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if _, err := client.Get("https://wrong-name.test"); err == nil {
		t.Fatal("TLS hostname verification bypassed")
	}
}

func TestDNSCancellationAndTransientErrors(t *testing.T) {
	p, _ := New([]Rule{{Origin: "https://receiver.test"}})
	p.resolver = lookupFunc(func(ctx context.Context, _, _ string) ([]netip.Addr, error) { <-ctx.Done(); return nil, ctx.Err() })
	client := &http.Client{Transport: p.Transport(), Timeout: 20 * time.Millisecond}
	if _, err := client.Get("https://receiver.test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unbounded DNS: %v", err)
	}
	temporary := &net.DNSError{IsTemporary: true}
	p, _ = New([]Rule{{Origin: "https://receiver.test"}})
	p.resolver = lookupFunc(func(context.Context, string, string) ([]netip.Addr, error) { return nil, temporary })
	// A new transport avoids sharing state with a dial winding down after cancellation.
	client = &http.Client{Transport: p.Transport(), Timeout: time.Second}
	_, err := client.Post("https://receiver.test", "application/json", strings.NewReader("{}"))
	if !errors.Is(err, temporary) || errors.Is(err, ErrDenied) {
		t.Fatalf("DNS error classification: %v", err)
	}
}
