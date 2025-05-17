package auth

import (
	"context"

	"github.com/erpc/erpc/common"
)

type ApiKeyStrategy struct {
	cfg  *common.ApiKeyStrategyConfig
	keys map[string]*common.ApiKeyConfig
}

func NewApiKeyStrategy(cfg *common.ApiKeyStrategyConfig) (AuthStrategy, error) {
	if cfg == nil || len(cfg.Keys) == 0 {
		return nil, common.NewErrInvalidConfig("API key strategy config is nil or no keys are defined")
	}
	keysMap := make(map[string]*common.ApiKeyConfig, len(cfg.Keys))
	for _, keyCfg := range cfg.Keys {
		if keyCfg.Value == "" {
			return nil, common.NewErrInvalidConfig("API key config has an empty value")
		}
		if keyCfg.Id == "" {
			return nil, common.NewErrInvalidConfig("API key config must have an 'id'")
		}
		keysMap[keyCfg.Value] = keyCfg
	}
	return &ApiKeyStrategy{
		cfg:  cfg,
		keys: keysMap,
	}, nil
}

func (s *ApiKeyStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeApiKey && ap.ApiKey != nil
}

func (s *ApiKeyStrategy) Authenticate(ctx context.Context, ap *AuthPayload) error {
	if ap.ApiKey == nil || ap.ApiKey.Value == "" {
		return common.NewErrAuthUnauthorized(string(common.AuthTypeApiKey), "API key not provided")
	}

	keyConfig, found := s.keys[ap.ApiKey.Value]
	if !found {
		return common.NewErrAuthUnauthorized(string(common.AuthTypeApiKey), "invalid API key")
	}

	// API key is valid, store its ID in the payload for later use (e.g., metrics)
	ap.ApiKey.Id = keyConfig.Id

	return nil
}
