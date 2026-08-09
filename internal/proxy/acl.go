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

type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type approvedDestination struct {
	original  string
	addresses []string
}

func (d approvedDestination) matches(address string) bool {
	originalHost, originalPort, originalErr := net.SplitHostPort(d.original)
	addressHost, addressPort, addressErr := net.SplitHostPort(address)
	if originalErr != nil || addressErr != nil || originalPort != addressPort {
		return false
	}
	originalHost = strings.TrimSuffix(strings.ToLower(originalHost), ".")
	addressHost = strings.TrimSuffix(strings.ToLower(addressHost), ".")
	return originalHost == addressHost
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
	_, err := a.approve(ctx, "tcp", address)
	return err
}

func (a *destinationACL) approve(ctx context.Context, network, address string) (approvedDestination, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" {
		return approvedDestination{}, fmt.Errorf("invalid destination %q", address)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return approvedDestination{}, fmt.Errorf("invalid destination port in %q", address)
	}
	if _, blocked := a.blockedPorts[port]; blocked {
		return approvedDestination{}, fmt.Errorf("%w: port %d is blocked", errDestinationDenied, port)
	}
	if a.allowPrivate {
		return approvedDestination{original: address, addresses: []string{address}}, nil
	}

	normalizedHost := strings.TrimSuffix(strings.ToLower(host), ".")
	if normalizedHost == "localhost" || strings.HasSuffix(normalizedHost, ".localhost") {
		return approvedDestination{}, fmt.Errorf("%w: localhost is blocked", errDestinationDenied)
	}
	if addressIP, parseErr := netip.ParseAddr(host); parseErr == nil {
		if err := checkDestinationIP(addressIP); err != nil {
			return approvedDestination{}, err
		}
		return approvedDestination{
			original:  address,
			addresses: []string{net.JoinHostPort(addressIP.Unmap().String(), portText)},
		}, nil
	}

	addresses, err := a.resolver.LookupNetIP(ctx, resolverNetwork(network), host)
	if err != nil {
		return approvedDestination{}, fmt.Errorf("resolve destination %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return approvedDestination{}, fmt.Errorf("resolve destination %q: no addresses", host)
	}
	approved := approvedDestination{original: address, addresses: make([]string, 0, len(addresses))}
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, addressIP := range addresses {
		addressIP = addressIP.Unmap()
		if err := checkDestinationIP(addressIP); err != nil {
			return approvedDestination{}, fmt.Errorf("destination %q: %w", host, err)
		}
		if _, duplicate := seen[addressIP]; duplicate {
			continue
		}
		seen[addressIP] = struct{}{}
		approved.addresses = append(approved.addresses, net.JoinHostPort(addressIP.String(), portText))
	}
	return approved, nil
}

func resolverNetwork(network string) string {
	switch network {
	case "tcp4":
		return "ip4"
	case "tcp6":
		return "ip6"
	default:
		return "ip"
	}
}

func (a *destinationACL) dial(
	ctx context.Context,
	dialer contextDialer,
	network string,
	address string,
) (net.Conn, error) {
	approved, err := a.approve(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return dialApproved(ctx, dialer, network, approved)
}

func dialApproved(
	ctx context.Context,
	dialer contextDialer,
	network string,
	destination approvedDestination,
) (net.Conn, error) {
	var dialErrors []error
	for _, address := range destination.addresses {
		connection, err := dialer.DialContext(ctx, network, address)
		if err == nil {
			return connection, nil
		}
		dialErrors = append(dialErrors, fmt.Errorf("%s: %w", address, err))
		if ctx.Err() != nil {
			break
		}
	}
	return nil, fmt.Errorf("dial destination %q: %w", destination.original, errors.Join(dialErrors...))
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
