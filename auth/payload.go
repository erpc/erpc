package auth

import "github.com/erpc/erpc/common"

type AuthPayload struct {
	Method string
	Type   common.AuthType
	Secret *SecretPayload
	Jwt    *JwtPayload
	Siwe   *SiwePayload
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

// NetworkPayload is no longer used; client IP is resolved at HTTP ingress and attached to the normalized request
