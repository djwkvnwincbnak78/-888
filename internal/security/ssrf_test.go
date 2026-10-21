package security

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
)

func TestHostAllowedPatterns(t *testing.T) {
	p := Policy{Hosts: []string{"api.openai.com", "*.anthropic.com", ".example.org"}}
	cases := map[string]bool{
		"api.openai.com":        true,
		"API.OPENAI.COM":        true,
		"api.openai.com.evil.io": false,
		"api.anthropic.com":     true,
		"a.b.anthropic.com":     true,
		"anthropic.com":         false,
		"x.example.org":         true,
		"example.org":           false,
		"evil.com":              false,
	}
	for host, want := range cases {
		if got := p.HostAllowed(host); got != want {
			t.Fatalf("HostAllowed(%q)=%v want %v", host, got, want)
		}
	}
}

func TestValidateURLRejectsDangerousInput(t *testing.T) {
	p := Policy{Hosts: []string{"api.openai.com", "169.254.169.254", "10.0.0.5", "127.0.0.1"}, Ports: []string{"443", "80"}}
	ctx := context.Background()

	cases := []struct {
		raw  string
		want error
	}{
		{"file:///etc/passwd", ErrSchemeNotAllowed},
		{"gopher://api.openai.com/", ErrSchemeNotAllowed},
		{"https://user:pass@api.openai.com/v1", ErrUserInfoRejected},
		{"https://evil.com/v1", ErrHostNotAllowed},
		{"https://169.254.169.254/latest/meta-data", ErrPrivateAddress},
		{"http://127.0.0.1:80/admin", ErrPrivateAddress},
		{"http://10.0.0.5/", ErrPrivateAddress},
		{"https://api.openai.com:8443/v1", ErrPortNotAllowed},
	}
	for _, tc := range cases {
		_, err := p.ValidateURL(ctx, tc.raw)
		if !errors.Is(err, tc.want) {
			t.Fatalf("ValidateURL(%q) err=%v want %v", tc.raw, err, tc.want)
		}
	}
}

func TestValidateURLAcceptsLiteralAllowedHost(t *testing.T) {
	p := Policy{Hosts: []string{"93.184.216.34"}, Ports: []string{"443"}}
	u, err := p.ValidateURL(context.Background(), "https://93.184.216.34/v1/models")
	if err != nil {
		t.Fatalf("expected allowed literal IP, got %v", err)
	}
	if u.Hostname() != "93.184.216.34" {
		t.Fatalf("host=%s", u.Hostname())
	}
}

func TestDialContextBlocksPrivateBeforeDial(t *testing.T) {
	p := Policy{Hosts: []string{"10.1.2.3", "localhost"}, Ports: []string{"443"}}
	_, err := p.DialContext(context.Background(), "tcp", "10.1.2.3:443")
	if !errors.Is(err, ErrPrivateAddress) {
		t.Fatalf("err=%v want ErrPrivateAddress", err)
	}
	_, err = p.DialContext(context.Background(), "tcp", "localhost:443")
	if err == nil {
		t.Fatal("localhost must not be dialable")
	}
}

func TestDialContextRejectsNonAllowlistedPort(t *testing.T) {
	p := Policy{Hosts: []string{"api.openai.com"}, Ports: []string{"443"}}
	_, err := p.DialContext(context.Background(), "tcp", "api.openai.com:6379")
	if !errors.Is(err, ErrPortNotAllowed) {
		t.Fatalf("err=%v want ErrPortNotAllowed", err)
	}
}

func TestBlockedIPRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "0.0.0.0",
		"::1", "fc00::1", "fe80::1", "ff02::1",
	}
	for _, s := range blocked {
		if !blockedIP(net.ParseIP(s)) {
			t.Fatalf("%s should be blocked", s)
		}
	}
	allowed := []string{"93.184.216.34", "1.1.1.1", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if blockedIP(net.ParseIP(s)) {
			t.Fatalf("%s should be allowed", s)
		}
	}
}

func TestNewClientRedirectPolicy(t *testing.T) {
	p := Policy{Hosts: []string{"api.openai.com"}, Ports: []string{"443"}}
	c := p.NewClient(0, 2)
	req, err := http.NewRequest(http.MethodGet, "https://evil.com/steal", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if err := c.CheckRedirect(req, nil); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("err=%v want ErrHostNotAllowed", err)
	}
}
