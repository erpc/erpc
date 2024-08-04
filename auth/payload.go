package auth

import "github.com/erpc/erpc/common"

type AuthPayload struct {
	ProjectId string
	Type      common.AuthType
	Secret    *SecretPayload
	Jwt       *JwtPayload
	Siwe      *SiwePayload
	IP        *IPPayload
}

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

type IPPayload struct {
	Address        string
	ForwardProxies []string
}
