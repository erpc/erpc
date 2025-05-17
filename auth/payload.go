package auth

import "github.com/erpc/erpc/common"

type AuthPayload struct {
	Method  string
	Type    common.AuthType
	Secret  *SecretPayload
	Jwt     *JwtPayload
	Siwe    *SiwePayload
	Network *NetworkPayload
	ApiKey  *ApiKeyPayload
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

type NetworkPayload struct {
	Address        string
	ForwardProxies []string
}

type ApiKeyPayload struct {
	Value string
	Id    string
}
