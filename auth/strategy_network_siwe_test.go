package auth

import (
	"context"
	"crypto/ecdsa"
	"strings"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/spruceid/siwe-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireAuthReason(t *testing.T, err error, strategy, reason string) {
	t.Helper()
	require.Error(t, err)
	var authErr *common.ErrAuthUnauthorized
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, reason, string(authErr.Reason()))
	assert.Equal(t, strategy, authErr.Details["strategy"])
}

func requestFromIP(ip string) *common.NormalizedRequest {
	req := common.NewNormalizedRequest([]byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}`))
	if ip != "" {
		req.SetClientIP(ip)
	}
	return req
}

func TestNewNetworkStrategy_RejectsInvalidEntries(t *testing.T) {
	_, err := NewNetworkStrategy(&common.NetworkStrategyConfig{AllowedIPs: []string{"10.0.0.1", "not-an-ip"}})
	require.ErrorContains(t, err, "invalid IP address: not-an-ip")

	_, err = NewNetworkStrategy(&common.NetworkStrategyConfig{AllowedCIDRs: []string{"10.0.0.0/33"}})
	require.ErrorContains(t, err, "invalid CIDR: 10.0.0.0/33")
}

func TestNetworkStrategy_Authenticate(t *testing.T) {
	cfg := &common.NetworkStrategyConfig{
		AllowedIPs:      []string{"203.0.113.7", "2001:db8::1"},
		AllowedCIDRs:    []string{"10.1.0.0/16", "192.168.0.0/24"},
		RateLimitBudget: "net-budget",
	}

	tests := []struct {
		name       string
		cfg        *common.NetworkStrategyConfig
		req        *common.NormalizedRequest
		wantUser   string
		wantBudget string
		wantReason string
	}{
		{name: "nil request is rejected", cfg: cfg, req: nil, wantReason: "missing_request"},
		{name: "unresolved client ip is rejected", cfg: cfg, req: requestFromIP(""), wantReason: "invalid_client_ip"},
		{name: "garbage client ip is rejected", cfg: cfg, req: requestFromIP("not-an-ip"), wantReason: "invalid_client_ip"},
		{name: "exact ipv4 match uses ip as user", cfg: cfg, req: requestFromIP("203.0.113.7"), wantUser: "203.0.113.7", wantBudget: "net-budget"},
		{name: "exact ipv6 match", cfg: cfg, req: requestFromIP("2001:db8:0:0:0:0:0:1"), wantUser: "2001:db8::1", wantBudget: "net-budget"},
		{name: "cidr match uses cidr as user by default", cfg: cfg, req: requestFromIP("10.1.200.3"), wantUser: "10.1.0.0/16", wantBudget: "net-budget"},
		{
			name:     "cidr match uses client ip when ipAsUser is set",
			cfg:      &common.NetworkStrategyConfig{AllowedCIDRs: []string{"192.168.0.0/24"}, IPAsUser: true},
			req:      requestFromIP("192.168.0.42"),
			wantUser: "192.168.0.42",
		},
		{name: "ip outside every rule is rejected", cfg: cfg, req: requestFromIP("8.8.8.8"), wantReason: "ip_not_allowed"},
		{name: "loopback is rejected unless localhost is allowed", cfg: cfg, req: requestFromIP("127.0.0.1"), wantReason: "ip_not_allowed"},
		{
			name:       "ipv4 loopback allowed when allowLocalhost",
			cfg:        &common.NetworkStrategyConfig{AllowLocalhost: true, RateLimitBudget: "local"},
			req:        requestFromIP("127.0.0.1"),
			wantUser:   "127.0.0.1",
			wantBudget: "local",
		},
		{
			name:     "ipv6 loopback allowed when allowLocalhost",
			cfg:      &common.NetworkStrategyConfig{AllowLocalhost: true},
			req:      requestFromIP("::1"),
			wantUser: "::1",
		},
		{
			name:       "allowLocalhost does not admit non-loopback addresses",
			cfg:        &common.NetworkStrategyConfig{AllowLocalhost: true},
			req:        requestFromIP("10.0.0.1"),
			wantReason: "ip_not_allowed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewNetworkStrategy(tc.cfg)
			require.NoError(t, err)

			ap := &AuthPayload{Type: common.AuthTypeNetwork}
			require.True(t, s.Supports(ap))
			assert.False(t, s.Supports(&AuthPayload{Type: common.AuthTypeSecret}))

			user, err := s.Authenticate(context.Background(), tc.req, ap)
			if tc.wantReason != "" {
				requireAuthReason(t, err, "network", tc.wantReason)
				assert.Nil(t, user)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantUser, user.Id)
			assert.Equal(t, tc.wantBudget, user.RateLimitBudget)
		})
	}
}

type siweFixture struct {
	key     *ecdsa.PrivateKey
	address string
}

func newSiweFixture(t *testing.T) siweFixture {
	t.Helper()
	key, err := crypto.GenerateKey()
	require.NoError(t, err)
	return siweFixture{key: key, address: crypto.PubkeyToAddress(key.PublicKey).Hex()}
}

func (f siweFixture) message(t *testing.T, domain string, opts map[string]interface{}) string {
	t.Helper()
	msg, err := siwe.InitMessage(domain, f.address, "https://"+domain+"/login", siwe.GenerateNonce(), opts)
	require.NoError(t, err)
	return msg.String()
}

func signSiwe(t *testing.T, key *ecdsa.PrivateKey, message string) string {
	t.Helper()
	sig, err := crypto.Sign(accounts.TextHash([]byte(message)), key)
	require.NoError(t, err)
	sig[64] += 27
	return hexutil.Encode(sig)
}

func TestSiweStrategy_Authenticate(t *testing.T) {
	signer := newSiweFixture(t)
	other := newSiweFixture(t)
	s := NewSiweStrategy(&common.SiweStrategyConfig{
		AllowedDomains:  []string{"app.example.com", "dapp.example.org"},
		RateLimitBudget: "siwe-budget",
	})

	valid := signer.message(t, "app.example.com", map[string]interface{}{
		"expirationTime": time.Now().Add(time.Hour),
	})
	wrongDomain := signer.message(t, "evil.example.net", nil)
	expired := signer.message(t, "app.example.com", map[string]interface{}{
		"issuedAt":       time.Now().Add(-2 * time.Hour),
		"expirationTime": time.Now().Add(-time.Hour),
	})
	notYetValid := signer.message(t, "dapp.example.org", map[string]interface{}{
		"notBefore": time.Now().Add(time.Hour),
	})

	tests := []struct {
		name       string
		payload    *SiwePayload
		wantReason string
	}{
		{name: "missing payload", payload: nil, wantReason: "missing_payload"},
		{name: "unparseable message", payload: &SiwePayload{Message: "hello world", Signature: "0x00"}, wantReason: "parse_failed"},
		{name: "signature from a different key", payload: &SiwePayload{Message: valid, Signature: signSiwe(t, other.key, valid)}, wantReason: "invalid_signature"},
		{name: "malformed signature", payload: &SiwePayload{Message: valid, Signature: "0xzz"}, wantReason: "invalid_signature"},
		{name: "domain not in allow list", payload: &SiwePayload{Message: wrongDomain, Signature: signSiwe(t, signer.key, wrongDomain)}, wantReason: "domain_not_allowed"},
		{name: "expired message", payload: &SiwePayload{Message: expired, Signature: signSiwe(t, signer.key, expired)}, wantReason: "message_not_valid_now"},
		{name: "not-before in the future", payload: &SiwePayload{Message: notYetValid, Signature: signSiwe(t, signer.key, notYetValid)}, wantReason: "message_not_valid_now"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ap := &AuthPayload{Type: common.AuthTypeSiwe, Siwe: tc.payload}
			require.True(t, s.Supports(ap))
			user, err := s.Authenticate(context.Background(), nil, ap)
			requireAuthReason(t, err, "siwe", tc.wantReason)
			assert.Nil(t, user)
		})
	}

	t.Run("valid signature authenticates the lowercased signer address", func(t *testing.T) {
		user, err := s.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeSiwe,
			Siwe: &SiwePayload{Message: valid, Signature: signSiwe(t, signer.key, valid)},
		})
		require.NoError(t, err)
		assert.Equal(t, strings.ToLower(signer.address), user.Id)
		assert.Equal(t, "siwe-budget", user.RateLimitBudget)
	})

	t.Run("no budget configured leaves user budget empty", func(t *testing.T) {
		plain := NewSiweStrategy(&common.SiweStrategyConfig{AllowedDomains: []string{"app.example.com"}})
		user, err := plain.Authenticate(context.Background(), nil, &AuthPayload{
			Type: common.AuthTypeSiwe,
			Siwe: &SiwePayload{Message: valid, Signature: signSiwe(t, signer.key, valid)},
		})
		require.NoError(t, err)
		assert.Empty(t, user.RateLimitBudget)
		assert.False(t, plain.Supports(&AuthPayload{Type: common.AuthTypeJwt}))
	})
}

func TestSecretStrategy_AssignsConfiguredBudget(t *testing.T) {
	s := NewSecretStrategy(&common.SecretStrategyConfig{Id: "ops", Value: "pw", RateLimitBudget: "ops-budget"})
	assert.False(t, s.Supports(&AuthPayload{Type: common.AuthTypeJwt}))
	user, err := s.Authenticate(context.Background(), nil, &AuthPayload{Type: common.AuthTypeSecret, Secret: &SecretPayload{Value: "pw"}})
	require.NoError(t, err)
	assert.Equal(t, "ops", user.Id)
	assert.Equal(t, "ops-budget", user.RateLimitBudget)
}
