// Package destination restricts webhook egress before requests and at TCP dial.
package destination

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var ErrDenied = errors.New("destination denied")

// Rule approves an exact origin. Public addresses are allowed; private and
// loopback addresses require an additional explicit prefix. No wildcards.
type Rule struct {
	Origin          string   `json:"origin"`
	PrivateNetworks []string `json:"private_networks,omitempty"`
}

type rule struct{ networks []netip.Prefix }
type resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}
type Policy struct {
	rules    map[string]rule
	resolver resolver
	dial     func(context.Context, string, string) (net.Conn, error)
}

func Load(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read destination policy")
	}
	var rules []Rule
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rules); err != nil {
		return nil, errors.New("invalid destination policy JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid destination policy JSON")
	}
	return New(rules)
}

func New(rules []Rule) (*Policy, error) {
	p := &Policy{rules: map[string]rule{}, resolver: net.DefaultResolver,
		dial: (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext}
	for _, input := range rules {
		u, key, err := origin(input.Origin)
		if err != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery {
			return nil, errors.New("invalid policy origin")
		}
		if _, exists := p.rules[key]; exists {
			return nil, errors.New("duplicate policy origin")
		}
		r := rule{}
		for _, text := range input.PrivateNetworks {
			prefix, err := netip.ParsePrefix(text)
			if err != nil || prefix != prefix.Masked() || !privatePrefix(prefix) {
				return nil, errors.New("invalid private network in policy")
			}
			r.networks = append(r.networks, prefix)
		}
		p.rules[key] = r
	}
	return p, nil
}

func origin(raw string) (*url.URL, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.Fragment != "" ||
		(u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, "", ErrDenied
	}
	host := strings.ToLower(u.Hostname())
	if address, err := netip.ParseAddr(host); err == nil {
		if address.Zone() != "" {
			return nil, "", ErrDenied
		}
		host = address.Unmap().String()
	} else {
		if len(host) > 253 {
			return nil, "", ErrDenied
		}
		for _, label := range strings.Split(host, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return nil, "", ErrDenied
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return nil, "", ErrDenied
				}
			}
		}
	}
	port := u.Port()
	if port == "" {
		if strings.HasSuffix(u.Host, ":") {
			return nil, "", ErrDenied
		}
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
		return nil, "", ErrDenied
	}
	return u, u.Scheme + "://" + net.JoinHostPort(host, port), nil
}

func (p *Policy) match(raw string) (*url.URL, rule, error) {
	u, key, err := origin(raw)
	if err != nil || p == nil {
		return nil, rule{}, ErrDenied
	}
	r, ok := p.rules[key]
	if !ok {
		return nil, rule{}, ErrDenied
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !r.allows(ip.Unmap()) {
		return nil, rule{}, ErrDenied
	}
	return u, r, nil
}

// Validate checks syntax, the approved origin, and literal IPs. Hostname
// resolution is deliberately repeated at each new connection, not trusted here.
func (p *Policy) Validate(raw string) error { _, _, err := p.match(raw); return err }

var privateSpaces = prefixes("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "fc00::/7", "::1/128")
var excludedPublic = prefixes("0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"168.63.129.16/32", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20")
var ipv6Global = netip.MustParsePrefix("2000::/3")

func prefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		result = append(result, netip.MustParsePrefix(v))
	}
	return result
}

func privatePrefix(p netip.Prefix) bool {
	for _, space := range privateSpaces {
		if space.Contains(p.Addr()) && p.Bits() >= space.Bits() {
			return true
		}
	}
	return false
}

func (r rule) allows(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if ip == netip.MustParseAddr("fd00:ec2::254") {
		return false
	}
	for _, space := range privateSpaces {
		if space.Contains(ip) {
			for _, allowed := range r.networks {
				if allowed.Contains(ip) {
					return true
				}
			}
			return false
		}
	}
	if !ip.IsGlobalUnicast() || ip.Is6() && !ipv6Global.Contains(ip) {
		return false
	}
	for _, space := range excludedPublic {
		if space.Contains(ip) {
			return false
		}
	}
	return true
}

type ruleKey struct{}

type transport struct {
	policy *Policy
	http   *http.Transport
}

// Transport owns its connection pool. Policy is immutable for that pool's life.
// Proxy environment variables cannot bypass the checked direct dial.
func (p *Policy) Transport() http.RoundTripper {
	return &transport{policy: p, http: &http.Transport{
		DialContext: p.dialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second,
		MaxIdleConns:        100, IdleConnTimeout: 90 * time.Second, ForceAttemptHTTP2: true,
	}}
}

func (t *transport) RoundTrip(request *http.Request) (*http.Response, error) {
	_, r, err := t.policy.match(request.URL.String())
	if err != nil {
		return nil, ErrDenied
	}
	return t.http.RoundTrip(request.Clone(context.WithValue(request.Context(), ruleKey{}, r)))
}

func (t *transport) CloseIdleConnections() { t.http.CloseIdleConnections() }

func (p *Policy) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	// net/http can let a pooled-connection dial outlive its initiating request.
	// Bound the entire DNS + connect operation, independently of Client.Timeout.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, ok := ctx.Value(ruleKey{}).(rule)
	if !ok || p == nil {
		return nil, ErrDenied
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, ErrDenied
	}
	var ips []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		ips = []netip.Addr{ip}
	} else {
		ips, err = p.resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
	}
	if len(ips) == 0 {
		return nil, &net.DNSError{IsNotFound: true}
	}
	// Reject mixed allowed/denied answers rather than falling back to a private IP.
	for _, ip := range ips {
		if !r.allows(ip) {
			return nil, ErrDenied
		}
	}
	for _, ip := range ips {
		var conn net.Conn
		conn, err = p.dial(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, err
}
