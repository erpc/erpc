package auth

import (
	"context"
	"fmt"
	"net"

	"github.com/erpc/erpc/common"
)

type NetworkStrategy struct {
	cfg          *common.NetworkStrategyConfig
	allowedIPs   []*net.IP
	allowedCIDRs []*net.IPNet
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

	return s, nil
}

func (s *NetworkStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeNetwork
}

func (s *NetworkStrategy) Authenticate(ctx context.Context, req *common.NormalizedRequest, ap *AuthPayload) (*common.User, error) {
	// Use the client IP resolved by the HTTP ingress and stored on the normalized request
	if req == nil {
		return nil, common.NewErrAuthUnauthorized("network", "request context is missing")
	}
	ipStr := req.ClientIP()
	clientIP := net.ParseIP(ipStr)
	if clientIP == nil {
		return nil, common.NewErrAuthUnauthorized("network", fmt.Sprintf("invalid or missing client IP: %s", ipStr))
	}

	// Check if localhost is allowed
	if s.cfg.AllowLocalhost && isLocalhost(clientIP) {
		user := &common.User{Id: clientIP.String()}
		if s.cfg.RateLimitBudget != "" {
			user.RateLimitBudget = s.cfg.RateLimitBudget
		}
		return user, nil
	}

	// Check against allowed IPs
	for _, ip := range s.allowedIPs {
		if clientIP.Equal(*ip) {
			user := &common.User{Id: clientIP.String()}
			if s.cfg.RateLimitBudget != "" {
				user.RateLimitBudget = s.cfg.RateLimitBudget
			}
			return user, nil
		}
	}

	// Check against allowed CIDRs
	for _, cidr := range s.allowedCIDRs {
		if cidr.Contains(clientIP) {
			user := &common.User{Id: clientIP.String()}
			if s.cfg.RateLimitBudget != "" {
				user.RateLimitBudget = s.cfg.RateLimitBudget
			}
			return user, nil
		}
	}

	return nil, common.NewErrAuthUnauthorized("network", fmt.Sprintf("IP %s is not allowed", clientIP.String()))
}

func isLocalhost(ip net.IP) bool {
	return ip.IsLoopback() || ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback)
}
