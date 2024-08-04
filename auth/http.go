package auth

import (
	"net/http"
	"strings"

	"github.com/erpc/erpc/common"
)

func NewPayloadFromHttp(r *http.Request) *AuthPayload {
	ap := &AuthPayload{
		Type: common.AuthNone,
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
			// parse basic auth and take the password
			parts := strings.Split(auth, " ")
			if len(parts) != 2 {
				return nil
			}
			ap.Type = common.AuthTypeSecret
			ap.Secret = &SecretPayload{
				Value: parts[1],
			}
		}
	} else if r.Header.Get("Authorization") != "" {
		ap.Type = common.AuthTypeJwt
		auth := strings.TrimSpace(r.Header.Get("Authorization"))
		label := strings.ToLower(auth[0:6])
		if strings.EqualFold(label, "bearer") {
			ap.Jwt = &JwtPayload{
				Token: auth[7:],
			}
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

	return ap
}
