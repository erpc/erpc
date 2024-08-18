package auth

import (
	"encoding/base64"
	"errors"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/valyala/fasthttp"
)

func NewPayloadFromHttp(projectId string, nq *common.NormalizedRequest, r *fasthttp.RequestCtx) (*AuthPayload, error) {
	method, _ := nq.Method()
	ap := &AuthPayload{
		ProjectId: projectId,
		Method:    method,
	}

	if r.QueryArgs().Has("token") {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: string(r.QueryArgs().Peek("token")),
		}
	} else if string(r.Request.Header.Peek("X-ERPC-Secret-Token")) != "" {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: string(r.Request.Header.Peek("X-ERPC-Secret-Token")),
		}
	} else if string(r.Request.Header.Peek("Authorization")) != "" {
		auth := strings.TrimSpace(string(r.Request.Header.Peek("Authorization")))
		label := strings.ToLower(auth[0:6])

		if strings.EqualFold(label, "basic") {
			basicAuthB64 := strings.TrimSpace(auth[6:])
			basicAuth, err := base64.StdEncoding.DecodeString(basicAuthB64)
			if err != nil {
				return nil, err
			}
			parts := strings.Split(string(basicAuth), ":")
			if len(parts) != 2 {
				return nil, errors.New("invalid basic auth must be base64 of username:password")
			}
			ap.Type = common.AuthTypeSecret
			ap.Secret = &SecretPayload{
				// Password will be considered the secret value provided
				// and username will be ignored.
				Value: parts[1],
			}
		} else if strings.EqualFold(label, "bearer") {
			ap.Type = common.AuthTypeJwt
			ap.Jwt = &JwtPayload{
				Token: auth[7:],
			}
		}
	} else if r.QueryArgs().Has("jwt") {
		ap.Type = common.AuthTypeJwt
		ap.Jwt = &JwtPayload{
			Token: string(r.QueryArgs().Peek("jwt")),
		}
	} else if r.QueryArgs().Has("signature") && r.QueryArgs().Has("message") {
		ap.Type = common.AuthTypeSiwe
		ap.Siwe = &SiwePayload{
			Signature: string(r.QueryArgs().Peek("signature")),
			Message:   normalizeSiweMessage(string(r.QueryArgs().Peek("message"))),
		}
	} else if string(r.Request.Header.Peek("X-Siwe-Message")) != "" && string(r.Request.Header.Peek("X-Siwe-Signature")) != "" {
		ap.Type = common.AuthTypeSiwe
		ap.Siwe = &SiwePayload{
			Signature: string(r.Request.Header.Peek("X-Siwe-Signature")),
			Message:   normalizeSiweMessage(string(r.Request.Header.Peek("X-Siwe-Message"))),
		}
	}

	// Add IP-based authentication
	if ap.Type == "" {
		ap.Type = common.AuthTypeNetwork
		ap.Network = &NetworkPayload{
			Address:        r.RemoteAddr().String(),
			ForwardProxies: strings.Split(string(r.Request.Header.Peek("X-Forwarded-For")), ","),
		}

	}

	return ap, nil
}

func normalizeSiweMessage(msg string) string {
	decoded, err := base64.StdEncoding.DecodeString(msg)
	if err != nil {
		return msg
	}
	return string(decoded)
}
