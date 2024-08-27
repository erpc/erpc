package auth

import (
	"encoding/base64"
	"errors"
	"strings"

	"github.com/erpc/erpc/common"
	"github.com/valyala/fasthttp"
)

func NewPayloadFromHttp(projectId string, nq *common.NormalizedRequest, headers *fasthttp.RequestHeader, args *fasthttp.Args) (*AuthPayload, error) {
	method, _ := nq.Method()
	ap := &AuthPayload{
		ProjectId: projectId,
		Method:    method,
	}

	if args.Has("token") {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: string(args.Peek("token")),
		}
	} else if tkn := headers.Peek("X-ERPC-Secret-Token"); tkn != nil {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: string(tkn),
		}
	} else if ath := headers.Peek("Authorization"); ath != nil {
		ath := strings.TrimSpace(string(ath))
		label := strings.ToLower(ath[0:6])

		if strings.EqualFold(label, "basic") {
			basicAuthB64 := strings.TrimSpace(ath[6:])
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
				Token: ath[7:],
			}
		}
	} else if args.Has("jwt") {
		ap.Type = common.AuthTypeJwt
		ap.Jwt = &JwtPayload{
			Token: string(args.Peek("jwt")),
		}
	} else if args.Has("signature") && args.Has("message") {
		ap.Type = common.AuthTypeSiwe
		ap.Siwe = &SiwePayload{
			Signature: string(args.Peek("signature")),
			Message:   normalizeSiweMessage(string(args.Peek("message"))),
		}
	} else if msg := headers.Peek("X-Siwe-Message"); msg != nil {
		if sig := headers.Peek("X-Siwe-Signature"); sig != nil {
			ap.Type = common.AuthTypeSiwe
			ap.Siwe = &SiwePayload{
				Signature: string(sig),
				Message:   normalizeSiweMessage(string(msg)),
			}
		}
	}

	// Add IP-based authentication
	if ap.Type == "" {
		ap.Type = common.AuthTypeNetwork
		ap.Network = &NetworkPayload{
			Address:        string(headers.Peek("X-Forwarded-For")),
			ForwardProxies: strings.Split(string(headers.Peek("X-Forwarded-For")), ","),
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
