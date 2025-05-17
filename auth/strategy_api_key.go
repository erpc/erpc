package auth

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
)

type ApiKeyStrategy struct {
	cfg        *common.ApiKeyStrategyConfig
	keyToIDMap map[string]string
}

func NewApiKeyStrategy(cfg *common.ApiKeyStrategyConfig) (*ApiKeyStrategy, error) {
	if cfg == nil {
		return nil, fmt.Errorf("API key strategy config is nil")
	}

	keyToIDMap := make(map[string]string, len(cfg.Keys))
	for _, key := range cfg.Keys {
		keyToIDMap[key.Value] = key.Id
	}

	return &ApiKeyStrategy{
		cfg:        cfg,
		keyToIDMap: keyToIDMap,
	}, nil
}

func (s *ApiKeyStrategy) Supports(ap *AuthPayload) bool {
	return ap.ApiKey.Value != ""
}

func (s *ApiKeyStrategy) Authenticate(ctx context.Context, ap *AuthPayload) error {
	for _, key := range s.cfg.Keys {
		if key.Value == ap.ApiKey.Value {
			return nil
		}
	}
	return common.NewErrAuthUnauthorized("apiKey", "invalid API key")
}

func (s *ApiKeyStrategy) GetKeyID(ap *AuthPayload) string {
	if id, exists := s.keyToIDMap[ap.ApiKey.Value]; exists {
		return id
	}
	return ""
}
