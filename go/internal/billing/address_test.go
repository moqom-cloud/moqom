package billing

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestClientAddress(t *testing.T) {
	cases := []struct {
		name, remote string
		forwarded    []string
		want         string
	}{
		{"direct", "203.0.113.7:5000", nil, "203.0.113.7"},
		{"direct caller cannot claim another address", "203.0.113.7:5000", []string{"198.51.100.1, 35.191.0.9"}, "203.0.113.7"},
		{"behind the balancer", "35.191.4.2:40000", []string{"198.51.100.1, 34.107.228.246"}, "198.51.100.1"},
		{"forged entries ahead of the balancer's", "130.211.1.1:40000", []string{"10.0.0.1, 1.2.3.4, 198.51.100.1, 34.107.228.246"}, "198.51.100.1"},
		{"split across headers", "35.191.4.2:40000", []string{"6.6.6.6", "198.51.100.1, 34.107.228.246"}, "198.51.100.1"},
		{"balancer without the header", "35.191.4.2:40000", nil, "35.191.4.2"},
		{"garbage in the client slot", "35.191.4.2:40000", []string{"nonsense, 34.107.228.246"}, "35.191.4.2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/signup", nil)
			r.RemoteAddr = c.remote
			for _, value := range c.forwarded {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := clientAddress(r); got != c.want {
				t.Fatalf("clientAddress = %q, want %q", got, c.want)
			}
		})
	}
}

func TestClientAddressTrustedProxies(t *testing.T) {
	TrustProxies([]netip.Prefix{netip.MustParsePrefix("10.21.0.0/16")})
	t.Cleanup(func() { trustedProxies.Store(nil) })
	cases := []struct {
		name, remote string
		forwarded    []string
		want         string
	}{
		{"behind the edge", "10.21.3.4:40000", []string{"198.51.100.1"}, "198.51.100.1"},
		{"the edge's entry is the last", "10.21.3.4:40000", []string{"6.6.6.6, 198.51.100.1"}, "198.51.100.1"},
		{"edge without the header", "10.21.3.4:40000", nil, "10.21.3.4"},
		{"garbage from the edge", "10.21.3.4:40000", []string{"nonsense"}, "10.21.3.4"},
		{"outside the range is not believed", "10.22.0.1:40000", []string{"198.51.100.1"}, "10.22.0.1"},
		{"Google's balancer still works", "35.191.4.2:40000", []string{"198.51.100.1, 34.107.228.246"}, "198.51.100.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/signup", nil)
			r.RemoteAddr = c.remote
			for _, value := range c.forwarded {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := clientAddress(r); got != c.want {
				t.Fatalf("clientAddress = %q, want %q", got, c.want)
			}
		})
	}
}
