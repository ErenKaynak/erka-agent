package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"
)

// ParseTrustedNetworks parses CIDR strings such as a WireGuard or Tailscale
// range.
func ParseTrustedNetworks(values []string) ([]*net.IPNet, error) {
	networks := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("invalid trusted network %q: %w", value, err)
		}
		networks = append(networks, network)
	}
	return networks, nil
}

func InTrustedNetwork(ip net.IP, networks []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// RestrictNetworks rejects connections whose peer address is outside the
// trusted networks before any authentication runs. It uses the TCP peer
// address and ignores forwarding headers, which clients control.
func RestrictNetworks(next http.Handler, networks []*net.IPNet) http.Handler {
	if len(networks) == 0 {
		return next
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		host, _, err := net.SplitHostPort(request.RemoteAddr)
		if err != nil || !InTrustedNetwork(net.ParseIP(host), networks) {
			writeAPIError(response, http.StatusForbidden, "untrusted_network", "Connections are accepted only from trusted networks")
			return
		}
		next.ServeHTTP(response, request)
	})
}
