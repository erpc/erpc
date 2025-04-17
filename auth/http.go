package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/erpc/erpc/common"
)

func NewPayloadFromHttp(method string, remoteAddr string, headers http.Header, args url.Values) (*AuthPayload, error) {
	ap := &AuthPayload{
		Method: method,
	}

	if token := args.Get("token"); token != "" { // deprecated
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: token,
		}
	} else if secret := args.Get("secret"); secret != "" {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: secret,
		}
	} else if tkn := headers.Get("X-ERPC-Secret-Token"); tkn != "" {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: tkn,
		}
	} else if ath := headers.Get("Authorization"); ath != "" {
		ath = strings.TrimSpace(ath)
		parts := strings.SplitN(ath, " ", 2)
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
				ap.Secret = &SecretPayload{
					// Password is considered the secret value; username is ignored.
					Value: creds[1],
				}
			} else if authType == "bearer" {
				ap.Type = common.AuthTypeJwt
				ap.Jwt = &JwtPayload{
					Token: authValue,
				}
			}
		}
	} else if jwt := args.Get("jwt"); jwt != "" {
		ap.Type = common.AuthTypeJwt
		ap.Jwt = &JwtPayload{
			Token: jwt,
		}
	} else if signature := args.Get("signature"); signature != "" && args.Get("message") != "" {
		ap.Type = common.AuthTypeSiwe
		ap.Siwe = &SiwePayload{
			Signature: signature,
			Message:   normalizeSiweMessage(args.Get("message")),
		}
	} else if msg := headers.Get("X-Siwe-Message"); msg != "" {
		if sig := headers.Get("X-Siwe-Signature"); sig != "" {
			ap.Type = common.AuthTypeSiwe
			ap.Siwe = &SiwePayload{
				Signature: sig,
				Message:   normalizeSiweMessage(msg),
			}
		}
	}

	// Add IP-based authentication
	if ap.Type == "" {
		xff := headers.Get("X-Forwarded-For")
		ap.Type = common.AuthTypeNetwork
		ap.Network = &NetworkPayload{
			Address:        remoteAddr,
			ForwardProxies: strings.Split(xff, ","),
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
