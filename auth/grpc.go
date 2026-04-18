package auth

import (
	"encoding/base64"
	"errors"
	"strings"

	"github.com/erpc/erpc/common"
	"google.golang.org/grpc/metadata"
)

func NewPayloadFromGrpc(method string, md metadata.MD) (*AuthPayload, error) {
	ap := &AuthPayload{Method: method}

	if vals := md.Get("x-erpc-secret-token"); len(vals) > 0 {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{Value: vals[0]}
	} else if vals := md.Get("authorization"); len(vals) > 0 {
		authz := strings.TrimSpace(vals[0])
		parts := strings.SplitN(authz, " ", 2)
		if len(parts) == 2 {
			authType := strings.ToLower(parts[0])
			authValue := parts[1]
			if authType == "basic" {
				basicAuth, err := base64.StdEncoding.DecodeString(authValue)
				if err != nil {
					return nil, err
				}
				creds := strings.SplitN(string(basicAuth), ":", 2)
				if len(creds) != 2 {
					return nil, errors.New("invalid basic auth: must be base64 of username:password")
				}
				ap.Type = common.AuthTypeSecret
				ap.Secret = &SecretPayload{Value: creds[1]}
			} else if authType == "bearer" {
				ap.Type = common.AuthTypeJwt
				ap.Jwt = &JwtPayload{Token: authValue}
			}
		}
	} else if msg := md.Get("x-siwe-message"); len(msg) > 0 {
		if sig := md.Get("x-siwe-signature"); len(sig) > 0 {
			ap.Type = common.AuthTypeSiwe
			ap.Siwe = &SiwePayload{
				Signature: sig[0],
				Message:   normalizeSiweMessage(msg[0]),
			}
		}
	}

	if ap.Type == "" {
		ap.Type = common.AuthTypeNetwork
	}
	return ap, nil
}
