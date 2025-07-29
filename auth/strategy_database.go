package auth

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
)

type DatabaseStrategy struct {
	logger    *zerolog.Logger
	cfg       *common.DatabaseStrategyConfig
	connector data.Connector
}

var _ AuthStrategy = &DatabaseStrategy{}

func NewDatabaseStrategy(appCtx context.Context, logger *zerolog.Logger, cfg *common.DatabaseStrategyConfig) (*DatabaseStrategy, error) {
	if cfg == nil {
		return nil, fmt.Errorf("database strategy config is nil")
	}

	if cfg.Connector == nil {
		return nil, fmt.Errorf("database strategy connector config is nil")
	}

	connector, err := data.NewConnector(appCtx, logger, cfg.Connector)
	if err != nil {
		return nil, fmt.Errorf("failed to create database connector: %w", err)
	}

	return &DatabaseStrategy{
		logger:    logger,
		cfg:       cfg,
		connector: connector,
	}, nil
}

func (s *DatabaseStrategy) Supports(ap *AuthPayload) bool {
	return ap.Type == common.AuthTypeSecret || ap.Type == common.AuthTypeDatabase
}

func (s *DatabaseStrategy) Authenticate(ctx context.Context, ap *AuthPayload) (*common.User, error) {
	if ap.Secret == nil {
		return nil, common.NewErrAuthUnauthorized("database", "no secret provided")
	}

	apiKey := ap.Secret.Value
	if apiKey == "" {
		return nil, common.NewErrAuthUnauthorized("database", "empty API key")
	}

	// Use API key as partition key and wildcard "*" as range key to get the record for this API key
	// (optimized: apiKey -> userId -> data, where userId is now the range key)
	rangeKey := "*"
	valueBytes, err := s.connector.Get(ctx, data.ConnectorMainIndex, apiKey, rangeKey)
	if err != nil {
		// Check for record not found error
		if common.HasErrorCode(err, "ErrRecordNotFound") {
			return nil, common.NewErrAuthUnauthorized("database", "invalid API key")
		}
		s.logger.Error().Err(err).Str("apiKey", apiKey).Msg("database query failed during authentication")
		return nil, common.NewErrAuthUnauthorized("database", fmt.Sprintf("database query failed: %v", err))
	}

	// Parse the JSON value
	var userData struct {
		UserId             string `json:"userId"`
		PerSecondRateLimit *int64 `json:"perSecondRateLimit,omitempty"`
		Enabled            *bool  `json:"enabled,omitempty"`
	}

	if err := json.Unmarshal(valueBytes, &userData); err != nil {
		s.logger.Error().Err(err).Str("apiKey", apiKey).RawJSON("data", valueBytes).Msg("failed to parse user data from database")
		return nil, common.NewErrAuthUnauthorized("database", "invalid user data format")
	}

	if userData.UserId == "" {
		s.logger.Error().Str("apiKey", apiKey).RawJSON("data", valueBytes).Msg("missing user ID in database record")
		return nil, common.NewErrAuthUnauthorized("database", "missing user ID in data")
	}

	// Check if API key is enabled (default to true if not specified for backward compatibility)
	enabled := true
	if userData.Enabled != nil {
		enabled = *userData.Enabled
	}

	if !enabled {
		s.logger.Warn().Str("apiKey", apiKey).Str("userId", userData.UserId).Msg("authentication attempt with disabled API key")
		return nil, common.NewErrAuthUnauthorized("database", "API key is disabled")
	}

	// Create and return the user
	user := &common.User{
		Id: userData.UserId,
	}

	if userData.PerSecondRateLimit != nil {
		user.PerSecondRateLimit = *userData.PerSecondRateLimit
	}

	s.logger.Debug().Str("apiKey", apiKey).Str("userId", user.Id).Int64("rateLimit", user.PerSecondRateLimit).Msg("user authenticated successfully from database")

	return user, nil
}

// GetConnector returns the database connector for admin operations
func (s *DatabaseStrategy) GetConnector() data.Connector {
	return s.connector
}
