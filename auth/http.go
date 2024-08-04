package auth

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

func NewPayloadFromHttp(projectId string, nq common.NormalizedRequest, r *http.Request) (*AuthPayload, error) {
	method, _ := nq.Method()
	ap := &AuthPayload{
		ProjectId: projectId,
		Method:    method,
	}

	if r.URL.Query().Get("token") != "" {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: r.URL.Query().Get("token"),
		}
	} else if r.Header.Get("X-ERPC-Secret-Token") != "" {
		ap.Type = common.AuthTypeSecret
		ap.Secret = &SecretPayload{
			Value: r.Header.Get("X-ERPC-Secret-Token"),
		}
	} else if r.Header.Get("Authorization") != "" {
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
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
	} else if r.URL.Query().Get("jwt") != "" {
		ap.Type = common.AuthTypeJwt
		ap.Jwt = &JwtPayload{
			Token: r.URL.Query().Get("jwt"),
		}
	} else if r.URL.Query().Get("signature") != "" && r.URL.Query().Get("message") != "" {
		ap.Type = common.AuthTypeSiwe
		ap.Siwe = &SiwePayload{
			Signature: r.URL.Query().Get("signature"),
			Message:   r.URL.Query().Get("message"),
		}
	} else if r.Header.Get("X-Siwe-Message") != "" && r.Header.Get("X-Siwe-Signature") != "" {
		ap.Type = common.AuthTypeSiwe
		ap.Siwe = &SiwePayload{
			Message:   r.Header.Get("X-Siwe-Message"),
			Signature: r.Header.Get("X-Siwe-Signature"),
		}
	}

	return ap, nil
}
