package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SSRF guard errors.
var (
	ErrSchemeNotAllowed = errors.New("ssrf: scheme not allowed")
	ErrHostNotAllowed   = errors.New("ssrf: host not in allowlist")
	ErrPortNotAllowed   = errors.New("ssrf: port not allowed")
	ErrPrivateAddress   = errors.New("ssrf: resolved address is private or reserved")
	ErrUserInfoRejected = errors.New("ssrf: userinfo in URL rejected")
	ErrTooManyRedirects = errors.New("ssrf: too many redirects")
	ErrEmptyHost        = errors.New("ssrf: empty host")
)

// Policy is an outbound allowlist. Hosts entries may be:
//
//	"api.openai.com"      exact host
//	"*.openai.com"        any single-or-multi-level subdomain
//	".example.com"        suffix match (subdomains only)
type Policy struct {
	Hosts           []string
	Ports           []string // default: 443 for https, 80 for http
	AllowPrivateIPs bool     // NEVER enable in production
	Resolver        *net.Resolver
}

// DefaultPolicy returns an allowlist for the upstream providers this gateway
// talks to. Override at startup with configuration.
func DefaultPolicy() Policy {
	return Policy{
		Hosts: []string{
			"api.openai.com",
			"api.anthropic.com",
		},
		Ports: []string{"443"},
	}
}

func (p Policy) resolver() *net.Resolver {
	if p.Resolver != nil {
		return p.Resolver
	}
	return net.DefaultResolver
}

func (p Policy) allowedPorts(scheme string) map[string]bool {
	out := make(map[string]bool)
	for _, port := range p.Ports {
		out[port] = true
	}
	if len(out) == 0 {
		if scheme == "https" {
			out["443"] = true
		} else {
			out["80"] = true
		}
	}
	return out
}

// HostAllowed reports whether host matches the allowlist.
func (p Policy) HostAllowed(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" {
		return false
	}
	for _, pattern := range p.Hosts {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		switch {
		case pattern == "":
			continue
		case strings.HasPrefix(pattern, "*."):
			suffix := pattern[1:] // ".openai.com"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		case strings.HasPrefix(pattern, "."):
			if strings.HasSuffix(host, pattern) && len(host) > len(pattern) {
				return true
			}
		default:
			if host == pattern {
				return true
			}
		}
	}
	return false
}

// blockedIP reports whether ip must never be dialled.
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() ||
		ip.IsPrivate() {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		// 100.64.0.0/10 CGNAT
		if v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
			return true
		}
		// 169.254.0.0/16 link-local (cloud metadata 169.254.169.254)
		if v4[0] == 169 && v4[1] == 254 {
			return true
		}
		// 192.0.0.0/24 IETF protocol assignments
		if v4[0] == 192 && v4[1] == 0 && v4[2] == 0 {
			return true
		}
		// 198.18.0.0/15 benchmarking
		if v4[0] == 198 && (v4[1] == 18 || v4[1] == 19) {
			return true
		}
		// 0.0.0.0/8
		if v4[0] == 0 {
			return true
		}
	}
	// IPv6 unique-local fc00::/7
	if len(ip) == net.IPv6len && (ip[0]&0xfe) == 0xfc {
		return true
	}
	return false
}

// ValidateURL performs the static + DNS checks for one outbound URL.
// It returns the parsed URL whose Host has been resolved and verified.
func (p Policy) ValidateURL(ctx context.Context, raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, ErrEmptyHost
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("ssrf: parse: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: %q", ErrSchemeNotAllowed, u.Scheme)
	}
	if u.User != nil {
		return nil, ErrUserInfoRejected
	}
	host := u.Hostname()
	if host == "" {
		return nil, ErrEmptyHost
	}

	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	if !p.allowedPorts(u.Scheme)[port] {
		return nil, fmt.Errorf("%w: %s", ErrPortNotAllowed, port)
	}

	if !p.HostAllowed(host) {
		return nil, fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
	}

	// If the host is a literal IP, the allowlist already approved it; still
	// apply the private-range filter unless explicitly permitted.
	if ip := net.ParseIP(host); ip != nil {
		if !p.AllowPrivateIPs && blockedIP(ip) {
			return nil, fmt.Errorf("%w: %s", ErrPrivateAddress, ip)
		}
		return u, nil
	}

	ips, err := p.resolver().LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("ssrf: dns lookup %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("ssrf: no address for %s", host)
	}
	for _, addr := range ips {
		if !p.AllowPrivateIPs && blockedIP(addr.IP) {
			return nil, fmt.Errorf("%w: %s -> %s", ErrPrivateAddress, host, addr.IP)
		}
	}
	return u, nil
}

// DialContext resolves and validates every connection at dial time. This is
// what defeats DNS rebinding: the address checked is the address dialled.
func (p Policy) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("ssrf: bad address %q: %w", address, err)
	}
	if !p.HostAllowed(host) {
		return nil, fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
	}
	if !p.allowedPorts("https")[port] && !p.allowedPorts("http")[port] {
		return nil, fmt.Errorf("%w: %s", ErrPortNotAllowed, port)
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

	if ip := net.ParseIP(host); ip != nil {
		if !p.AllowPrivateIPs && blockedIP(ip) {
			return nil, fmt.Errorf("%w: %s", ErrPrivateAddress, ip)
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}

	ips, err := p.resolver().LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("ssrf: dns lookup %s: %w", host, err)
	}
	var lastErr error
	for _, addr := range ips {
		if !p.AllowPrivateIPs && blockedIP(addr.IP) {
			lastErr = fmt.Errorf("%w: %s -> %s", ErrPrivateAddress, host, addr.IP)
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("ssrf: no dialable address for %s", host)
	}
	return nil, lastErr
}

// NewTransport builds an http.Transport that cannot be redirected into the
// internal network: no proxy from env, no keep-alive to unvalidated hosts,
// and the SSRF-checking dialer installed.
func (p Policy) NewTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 nil, // ignore HTTP_PROXY / HTTPS_PROXY
		DialContext:           p.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// NewClient returns an http.Client enforcing the policy on every redirect hop.
func (p Policy) NewClient(timeout time.Duration, maxRedirects int) *http.Client {
	// A zero timeout means "no global client timeout". A global Timeout covers
	// body reads and would sever long SSE streams; callers must bound each
	// request with context.WithDeadline instead.
	if maxRedirects <= 0 {
		maxRedirects = 3
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: p.NewTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return ErrTooManyRedirects
			}
			if _, err := p.ValidateURL(req.Context(), req.URL.String()); err != nil {
				return err
			}
			return nil
		},
	}
}
