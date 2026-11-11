package auth

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"testing"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
)

func TestNewPayloadFromHttp_SignalPrecedence(t *testing.T) {
	siweMsg := "example.com wants you to sign in"
	siweMsgB64 := base64.StdEncoding.EncodeToString([]byte(siweMsg))
	basic := func(creds string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
	}

	tests := []struct {
		name       string
		headers    http.Header
		args       url.Values
		wantType   common.AuthType
		wantSecret string
		wantJwt    string
		wantSig    string
		wantMsg    string
	}{
		{
			name:     "no signals defaults to network",
			wantType: common.AuthTypeNetwork,
		},
		{
			name:       "deprecated token query param wins over everything",
			args:       url.Values{"token": {"tok"}, "secret": {"sec"}, "jwt": {"j"}},
			headers:    http.Header{"X-Erpc-Secret-Token": {"hdr"}, "Authorization": {"Bearer b"}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "tok",
		},
		{
			name:       "secret query param wins over header",
			args:       url.Values{"secret": {"sec"}},
			headers:    http.Header{"X-Erpc-Secret-Token": {"hdr"}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "sec",
		},
		{
			name:       "secret header wins over authorization",
			headers:    http.Header{"X-Erpc-Secret-Token": {"hdr"}, "Authorization": {"Bearer b"}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "hdr",
		},
		{
			name:       "basic auth password is the secret, username ignored",
			headers:    http.Header{"Authorization": {basic("alice:pa:ss")}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "pa:ss",
		},
		{
			name:       "auth scheme is case-insensitive and value is trimmed",
			headers:    http.Header{"Authorization": {"  BASIC " + base64.StdEncoding.EncodeToString([]byte("u:p")) + "  "}},
			wantType:   common.AuthTypeSecret,
			wantSecret: "p",
		},
		{
			name:     "bearer token becomes jwt",
			headers:  http.Header{"Authorization": {"Bearer eyJ.abc.def"}},
			wantType: common.AuthTypeJwt,
			wantJwt:  "eyJ.abc.def",
		},
		{
			name:     "unknown authorization scheme falls back to network and shadows jwt query",
			headers:  http.Header{"Authorization": {"Digest abc"}},
			args:     url.Values{"jwt": {"ignored"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "authorization without a value falls back to network",
			headers:  http.Header{"Authorization": {"Bearer"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "jwt query param",
			args:     url.Values{"jwt": {"q.jwt.tok"}},
			wantType: common.AuthTypeJwt,
			wantJwt:  "q.jwt.tok",
		},
		{
			name:     "siwe query params with base64 message are decoded",
			args:     url.Values{"signature": {"0xsig"}, "message": {siweMsgB64}},
			wantType: common.AuthTypeSiwe,
			wantSig:  "0xsig",
			wantMsg:  siweMsg,
		},
		{
			name:     "siwe query params with plain message are kept verbatim",
			args:     url.Values{"signature": {"0xsig"}, "message": {siweMsg}},
			wantType: common.AuthTypeSiwe,
			wantSig:  "0xsig",
			wantMsg:  siweMsg,
		},
		{
			name:     "siwe signature without message is ignored",
			args:     url.Values{"signature": {"0xsig"}},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "siwe headers",
			headers:  http.Header{"X-Siwe-Message": {siweMsgB64}, "X-Siwe-Signature": {"0xhdr"}},
			wantType: common.AuthTypeSiwe,
			wantSig:  "0xhdr",
			wantMsg:  siweMsg,
		},
		{
			name:     "siwe message header without signature falls back to network",
			headers:  http.Header{"X-Siwe-Message": {siweMsgB64}},
			wantType: common.AuthTypeNetwork,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			headers := tc.headers
			if headers == nil {
				headers = http.Header{}
			}
			args := tc.args
			if args == nil {
				args = url.Values{}
			}
			ap, err := NewPayloadFromHttp("eth_call", "10.0.0.1:1234", headers, args)
			require.NoError(t, err)
			assert.Equal(t, "eth_call", ap.Method)
			assert.Equal(t, tc.wantType, ap.Type)

			if tc.wantSecret != "" {
				require.NotNil(t, ap.Secret)
				assert.Equal(t, tc.wantSecret, ap.Secret.Value)
			} else {
				assert.Nil(t, ap.Secret)
			}
			if tc.wantJwt != "" {
				require.NotNil(t, ap.Jwt)
				assert.Equal(t, tc.wantJwt, ap.Jwt.Token)
			} else {
				assert.Nil(t, ap.Jwt)
			}
			if tc.wantSig != "" {
				require.NotNil(t, ap.Siwe)
				assert.Equal(t, tc.wantSig, ap.Siwe.Signature)
				assert.Equal(t, tc.wantMsg, ap.Siwe.Message)
			} else {
				assert.Nil(t, ap.Siwe)
			}
		})
	}
}

func TestNewPayloadFromHttp_MalformedBasicAuth(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "not base64", value: "Basic !!!not-base64!!!"},
		{name: "no colon separator", value: "Basic " + base64.StdEncoding.EncodeToString([]byte("justuser"))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ap, err := NewPayloadFromHttp("eth_call", "", http.Header{"Authorization": {tc.value}}, url.Values{})
			require.Error(t, err)
			assert.Nil(t, ap)
		})
	}
}

func TestNewPayloadFromGrpc_Signals(t *testing.T) {
	siweMsg := "grpc.example wants you to sign in"
	tests := []struct {
		name       string
		md         metadata.MD
		wantType   common.AuthType
		wantSecret string
		wantSig    string
		wantMsg    string
		wantErr    bool
	}{
		{
			name:       "secret token metadata wins over authorization",
			md:         metadata.Pairs("x-erpc-secret-token", "s3", "authorization", "Bearer x"),
			wantType:   common.AuthTypeSecret,
			wantSecret: "s3",
		},
		{
			name:     "siwe metadata decodes base64 message",
			md:       metadata.Pairs("x-siwe-message", base64.StdEncoding.EncodeToString([]byte(siweMsg)), "x-siwe-signature", "0xabc"),
			wantType: common.AuthTypeSiwe,
			wantSig:  "0xabc",
			wantMsg:  siweMsg,
		},
		{
			name:     "siwe message without signature defaults to network",
			md:       metadata.Pairs("x-siwe-message", siweMsg),
			wantType: common.AuthTypeNetwork,
		},
		{
			name:     "empty metadata defaults to network",
			md:       metadata.MD{},
			wantType: common.AuthTypeNetwork,
		},
		{
			name:    "basic auth that is not base64 is rejected",
			md:      metadata.Pairs("authorization", "Basic %%%"),
			wantErr: true,
		},
		{
			name:    "basic auth without colon is rejected",
			md:      metadata.Pairs("authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("nocolon"))),
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ap, err := NewPayloadFromGrpc("eth_chainId", tc.md)
			if tc.wantErr {
				require.Error(t, err)
				assert.Nil(t, ap)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantType, ap.Type)
			if tc.wantSecret != "" {
				require.NotNil(t, ap.Secret)
				assert.Equal(t, tc.wantSecret, ap.Secret.Value)
			}
			if tc.wantSig != "" {
				require.NotNil(t, ap.Siwe)
				assert.Equal(t, tc.wantSig, ap.Siwe.Signature)
				assert.Equal(t, tc.wantMsg, ap.Siwe.Message)
			} else {
				assert.Nil(t, ap.Siwe)
			}
		})
	}
}
