package auth

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/erpc/erpc/common"
)

type NetworkStrategy struct {
	cfg            *common.NetworkStrategyConfig
	allowedIPs     []*net.IP
	allowedCIDRs   []*net.IPNet
	trustedProxies []*net.IPNet
}

var _ AuthStrategy = &NetworkStrategy{}

func NewNetworkStrategy(cfg *common.NetworkStrategyConfig) (*NetworkStrategy, error) {
	s := &NetworkStrategy{
		cfg: cfg,
	}

	// Parse and store allowed IPs
	for _, ipStr := range cfg.AllowedIPs {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP address: %s", ipStr)
		}
		s.allowedIPs = append(s.allowedIPs, &ip)
	}

	// Parse and store allowed CIDRs
	for _, cidrStr := range cfg.AllowedCIDRs {
		_, cidr, err := net.ParseCIDR(cidrStr)
		if err != nil {
			return nil, fmt.Errorf("invalid CIDR: %s", cidrStr)
		}
		s.allowedCIDRs = append(s.allowedCIDRs, cidr)
	}

	// Parse and store trusted proxies
	for _, proxyStr := range cfg.TrustedProxies {
		_, proxy, err := net.ParseCIDR(proxyStr)
		if err != nil {
			ip := net.ParseIP(proxyStr)
			if ip == nil {
				return nil, fmt.Errorf("invalid trusted proxy: %s", proxyStr)
			}
			proxy = &net.IPNet{IP: ip, Mask: net.CIDRMask(32, 32)}
		}
		s.trustedProxies = append(s.trustedProxies, proxy)
	}

	return s, nil
}

func (s *NetworkStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeNetwork
}

func (s *NetworkStrategy) Authenticate(ctx context.Context, ap *AuthPayload) (*common.User, error) {
	if ap.Network == nil {
		return nil, common.NewErrAuthUnauthorized("network", "missing network payload")
	}

	clientIP := s.determineClientIP(ap.Network)
	if clientIP == nil {
		return nil, common.NewErrAuthUnauthorized("network", "unable to determine client IP")
	}

	// Check if localhost is allowed
	if s.cfg.AllowLocalhost && isLocalhost(clientIP) {
		return &common.User{
			Id: clientIP.String(),
		}, nil
	}

	// Check against allowed IPs
	for _, ip := range s.allowedIPs {
		if clientIP.Equal(*ip) {
			return &common.User{
				Id: clientIP.String(),
			}, nil
		}
	}

	// Check against allowed CIDRs
	for _, cidr := range s.allowedCIDRs {
		if cidr.Contains(clientIP) {
			return &common.User{
				Id: clientIP.String(),
			}, nil
		}
	}

	return nil, common.NewErrAuthUnauthorized("network", fmt.Sprintf("IP %s is not allowed", clientIP.String()))
}

// determineClientIP extracts the actual client IP address from the NetworkPayload
// by checking X-Forwarded-For headers and falling back to RemoteAddr if needed.
// It uses the following algorithm:
// 1. Process X-Forwarded-For from right to left (most recent proxy to original client)
// 2. Return the first non-trusted-proxy IP (which should be the actual client)
// 3. Fall back to RemoteAddr if no client IP can be determined
func (s *NetworkStrategy) determineClientIP(np *NetworkPayload) net.IP {
	// First, check the X-Forwarded-For header
	for i := len(np.ForwardProxies) - 1; i >= 0; i-- {
		ipStr := strings.TrimSpace(np.ForwardProxies[i])
		if ipStr == "" {
			continue // Skip empty entries
		}
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue // Skip invalid IPs
		}
		if !s.isTrustedProxy(ip) {
			return ip // Found the client IP
		}
	}

	// If no valid IP found in X-Forwarded-For, use the RemoteAddr
	remoteIP, _, err := net.SplitHostPort(np.Address)
	if err != nil {
		// If splitting fails, try to parse the whole string as an IP
		return net.ParseIP(np.Address)
	}
	return net.ParseIP(remoteIP)
}

func (s *NetworkStrategy) isTrustedProxy(ip net.IP) bool {
	for _, proxy := range s.trustedProxies {
		if proxy.Contains(ip) {
			return true
		}
	}
	return false
}

func isLocalhost(ip net.IP) bool {
	return ip.IsLoopback() || ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback)
}
