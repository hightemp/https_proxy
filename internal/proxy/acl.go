package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"github.com/hightemp/https_proxy/internal/config"
)

var errDestinationDenied = errors.New("destination denied by policy")

var reservedDestinationPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001:db8::/32"),
}

type netIPResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type destinationACL struct {
	allowPrivate bool
	blockedPorts map[int]struct{}
	resolver     netIPResolver
}

func newDestinationACL(cfg config.Config) *destinationACL {
	blockedPorts := make(map[int]struct{}, len(cfg.BlockedDestinationPorts))
	for _, port := range cfg.BlockedDestinationPorts {
		blockedPorts[port] = struct{}{}
	}
	return &destinationACL{
		allowPrivate: cfg.AllowPrivateDestinations,
		blockedPorts: blockedPorts,
		resolver:     net.DefaultResolver,
	}
}

func (a *destinationACL) check(ctx context.Context, address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return fmt.Errorf("invalid destination %q", address)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid destination port in %q", address)
	}
	if _, blocked := a.blockedPorts[port]; blocked {
		return fmt.Errorf("%w: port %d is blocked", errDestinationDenied, port)
	}
	if a.allowPrivate {
		return nil
	}

	normalizedHost := strings.TrimSuffix(strings.ToLower(host), ".")
	if normalizedHost == "localhost" || strings.HasSuffix(normalizedHost, ".localhost") {
		return fmt.Errorf("%w: localhost is blocked", errDestinationDenied)
	}
	if addressIP, parseErr := netip.ParseAddr(host); parseErr == nil {
		return checkDestinationIP(addressIP)
	}

	addresses, err := a.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return fmt.Errorf("resolve destination %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return fmt.Errorf("resolve destination %q: no addresses", host)
	}
	for _, addressIP := range addresses {
		if err := checkDestinationIP(addressIP); err != nil {
			return fmt.Errorf("destination %q: %w", host, err)
		}
	}
	return nil
}

func checkDestinationIP(address netip.Addr) error {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return fmt.Errorf("%w: non-public address", errDestinationDenied)
	}
	for _, prefix := range reservedDestinationPrefixes {
		if prefix.Contains(address) {
			return fmt.Errorf("%w: reserved address", errDestinationDenied)
		}
	}
	return nil
}

func requestDestination(requestURL *url.URL) (string, error) {
	host := requestURL.Hostname()
	if host == "" {
		return "", errors.New("request destination host is empty")
	}
	port := requestURL.Port()
	if port == "" {
		switch strings.ToLower(requestURL.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		default:
			return "", fmt.Errorf("unsupported request scheme %q", requestURL.Scheme)
		}
	}
	return net.JoinHostPort(host, port), nil
}
