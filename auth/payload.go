package auth

import "github.com/erpc/erpc/common"

type AuthPayload struct {
	Method string
	Type   common.AuthType
	Secret *SecretPayload
	Jwt    *JwtPayload
	Siwe   *SiwePayload
	X402   *X402Payload
}

// This payload is used by both "secret" and "database" strategies
type SecretPayload struct {
	Value string
}

type JwtPayload struct {
	Token string
}

type SiwePayload struct {
	Signature string
	Message   string
}

// X402Payload carries the base64-encoded X-PAYMENT header value for x402 authentication.
type X402Payload struct {
	Payment string
}
