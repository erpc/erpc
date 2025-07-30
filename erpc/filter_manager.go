package erpc

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/rs/zerolog"
)

type FilterSession struct {
	FilterID     string    `json:"filterId"`
	UpstreamID   string    `json:"upstreamId"`
	NetworkID    string    `json:"networkId"`
	ProjectID    string    `json:"projectId"`
	Method       string    `json:"method"`
	CreatedAt    time.Time `json:"createdAt"`
	LastAccessAt time.Time `json:"lastAccessAt"`
}

type FilterManager struct {
	logger          *zerolog.Logger
	sharedState     data.SharedStateRegistry
	filterSessions  sync.Map // local cache of FilterSession
	sessionTimeout  time.Duration
	cleanupInterval time.Duration
	cleanupTicker   *time.Ticker
	stopCleanup     chan struct{}
	mu              sync.RWMutex
}

// FilterID patterns for different types of filters
var (
	filterIDPattern = regexp.MustCompile(`^0x[a-fA-F0-9]+$`)
)

func NewFilterManager(logger *zerolog.Logger, sharedState data.SharedStateRegistry) *FilterManager {
	lg := logger.With().Str("component", "filterManager").Logger()

	fm := &FilterManager{
		logger:          &lg,
		sharedState:     sharedState,
		sessionTimeout:  30 * time.Minute, // Filters expire after 30 minutes of inactivity
		cleanupInterval: 5 * time.Minute,  // Check for expired filters every 5 minutes
		stopCleanup:     make(chan struct{}),
	}

	// Start background cleanup routine
	fm.startCleanup()

	return fm
}

func (fm *FilterManager) startCleanup() {
	fm.mu.Lock()
	if fm.cleanupTicker != nil {
		fm.cleanupTicker.Stop()
	}
	fm.cleanupTicker = time.NewTicker(fm.cleanupInterval)
	ticker := fm.cleanupTicker
	fm.mu.Unlock()

	go func() {
		for {
			select {
			case <-ticker.C:
				fm.cleanupExpiredFilters()
			case <-fm.stopCleanup:
				return
			}
		}
	}()
}

func (fm *FilterManager) Stop() {
	fm.mu.Lock()
	defer fm.mu.Unlock()

	if fm.cleanupTicker != nil {
		fm.cleanupTicker.Stop()
		fm.cleanupTicker = nil
	}

	close(fm.stopCleanup)
}

func (fm *FilterManager) cleanupExpiredFilters() {
	fm.logger.Debug().Msg("cleaning up expired filter sessions")

	// Only clean local cache when shared state is not configured
	// When shared state is configured, Redis TTL handles expiration automatically
	if fm.sharedState == nil {
		now := time.Now()
		fm.filterSessions.Range(func(key, value interface{}) bool {
			session := value.(*FilterSession)
			if now.Sub(session.LastAccessAt) > fm.sessionTimeout {
				fm.filterSessions.Delete(key)
				fm.logger.Debug().
					Str("filterId", session.FilterID).
					Str("upstreamId", session.UpstreamID).
					Msg("expired filter session removed from local cache")
			}
			return true
		})
	} else {
		fm.logger.Debug().Msg("shared state configured - Redis TTL handles expiration, no local cleanup needed")
	}
}

// CreateFilterSession creates a new filter session and stores it locally and in shared state
func (fm *FilterManager) CreateFilterSession(ctx context.Context, filterId, upstreamId, networkId, projectId, method string) error {
	_, span := common.StartDetailSpan(ctx, "FilterManager.CreateFilterSession")
	defer span.End()

	// If shared state is configured, store only the upstream ID in Redis
	if fm.sharedState != nil {
		key := fm.getFilterKey(filterId, networkId, projectId)

		// Store upstream ID with TTL matching session timeout
		ttl := fm.sessionTimeout
		err := fm.sharedState.SetString(ctx, key, upstreamId, &ttl)
		if err != nil {
			fm.logger.Error().Err(err).Str("filterId", filterId).Str("key", key).Msg("failed to store filter session in shared state")
			return fmt.Errorf("failed to store filter session in shared state: %w", err)
		}

		fm.logger.Debug().Str("filterId", filterId).Str("upstreamId", upstreamId).Str("key", key).Msg("stored filter session in shared state")
	} else {
		// If no shared state configured, use local cache with full session object
		session := &FilterSession{
			FilterID:     filterId,
			UpstreamID:   upstreamId,
			NetworkID:    networkId,
			ProjectID:    projectId,
			Method:       method,
			CreatedAt:    time.Now(),
			LastAccessAt: time.Now(),
		}
		fm.filterSessions.Store(filterId, session)
		fm.logger.Debug().Str("filterId", filterId).Msg("stored filter session in local cache only (no shared state configured)")
	}

	fm.logger.Debug().
		Str("filterId", filterId).
		Str("upstreamId", upstreamId).
		Str("networkId", networkId).
		Str("method", method).
		Msg("created filter session")

	return nil
}

// GetFilterSession retrieves the upstream ID for a given filter
func (fm *FilterManager) GetFilterSession(ctx context.Context, filterId, networkId, projectId string) (*FilterSession, error) {
	_, span := common.StartDetailSpan(ctx, "FilterManager.GetFilterSession")
	defer span.End()

	// If shared state is configured, retrieve upstream ID from Redis
	if fm.sharedState != nil {
		key := fm.getFilterKey(filterId, networkId, projectId)
		upstreamId, err := fm.sharedState.GetString(ctx, key)
		if err != nil {
			if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
				fm.logger.Debug().Str("filterId", filterId).Str("key", key).Msg("filter session not found in shared state")
			} else {
				fm.logger.Error().Err(err).Str("filterId", filterId).Str("key", key).Msg("failed to retrieve filter session from shared state")
			}
			return nil, fmt.Errorf("filter session not found for filterId: %s", filterId)
		}

		// Create a minimal session object with just the upstream ID
		session := &FilterSession{
			FilterID:     filterId,
			UpstreamID:   upstreamId,
			NetworkID:    networkId,
			ProjectID:    projectId,
			LastAccessAt: time.Now(),
		}

		fm.logger.Debug().
			Str("filterId", filterId).
			Str("upstreamId", upstreamId).
			Str("key", key).
			Msg("retrieved filter session from shared state")

		return session, nil
	}

	// If no shared state configured, use local cache only
	if value, ok := fm.filterSessions.Load(filterId); ok {
		session := value.(*FilterSession)
		// Update last access time
		session.LastAccessAt = time.Now()
		fm.filterSessions.Store(filterId, session)

		fm.logger.Debug().
			Str("filterId", filterId).
			Str("upstreamId", session.UpstreamID).
			Msg("retrieved filter session from local cache")

		return session, nil
	}

	return nil, fmt.Errorf("filter session not found for filterId: %s", filterId)
}

// RemoveFilterSession removes a filter session when filter is uninstalled
func (fm *FilterManager) RemoveFilterSession(ctx context.Context, filterId, networkId, projectId string) error {
	_, span := common.StartDetailSpan(ctx, "FilterManager.RemoveFilterSession")
	defer span.End()

	// If shared state is configured, remove only from Redis
	if fm.sharedState != nil {
		key := fm.getFilterKey(filterId, networkId, projectId)
		err := fm.sharedState.DeleteString(ctx, key)
		if err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			fm.logger.Error().Err(err).Str("filterId", filterId).Str("key", key).Msg("failed to remove filter session from shared state")
			return fmt.Errorf("failed to remove filter session from shared state: %w", err)
		}

		fm.logger.Debug().Str("filterId", filterId).Str("key", key).Msg("removed filter session from shared state")
	} else {
		// If no shared state configured, remove from local cache only
		fm.filterSessions.Delete(filterId)
		fm.logger.Debug().Str("filterId", filterId).Msg("removed filter session from local cache only (no shared state configured)")
	}

	fm.logger.Debug().
		Str("filterId", filterId).
		Msg("removed filter session")

	return nil
}

// ExtendFilterSessionTTL extends the TTL of a filter session when it's successfully accessed
func (fm *FilterManager) ExtendFilterSessionTTL(ctx context.Context, filterId, networkId, projectId, upstreamId string) error {
	_, span := common.StartDetailSpan(ctx, "FilterManager.ExtendFilterSessionTTL")
	defer span.End()

	// Only extend TTL if shared state is configured (Redis)
	if fm.sharedState != nil {
		key := fm.getFilterKey(filterId, networkId, projectId)

		// Reset the TTL by storing the upstream ID with new TTL
		ttl := fm.sessionTimeout
		err := fm.sharedState.SetString(ctx, key, upstreamId, &ttl)
		if err != nil {
			fm.logger.Error().Err(err).Str("filterId", filterId).Str("key", key).Msg("failed to extend filter session TTL")
			return fmt.Errorf("failed to extend filter session TTL: %w", err)
		}

		fm.logger.Debug().
			Str("filterId", filterId).
			Str("upstreamId", upstreamId).
			Str("key", key).
			Dur("ttl", ttl).
			Msg("extended filter session TTL")
	} else {
		// For local cache, update the LastAccessAt time
		if value, ok := fm.filterSessions.Load(filterId); ok {
			session := value.(*FilterSession)
			session.LastAccessAt = time.Now()
			fm.filterSessions.Store(filterId, session)
		}
	}

	return nil
}

// IsFilterMethod checks if a method is filter-related (both creation and operation)
func (fm *FilterManager) IsFilterMethod(method string) bool {
	return fm.IsFilterCreationMethod(method) || fm.IsFilterOperationMethod(method)
}

// IsFilterCreationMethod checks if a method creates a new filter
func (fm *FilterManager) IsFilterCreationMethod(method string) bool {
	switch method {
	case "eth_newFilter", "eth_newBlockFilter", "eth_newPendingTransactionFilter":
		return true
	default:
		return false
	}
}

// IsFilterOperationMethod checks if a method operates on an existing filter (requires sticky routing)
func (fm *FilterManager) IsFilterOperationMethod(method string) bool {
	switch method {
	case "eth_getFilterChanges", "eth_uninstallFilter":
		return true
	default:
		return false
	}
}

// ExtractFilterIdFromRequest extracts filter ID from request parameters
func (fm *FilterManager) ExtractFilterIdFromRequest(ctx context.Context, req *common.NormalizedRequest) (string, error) {
	jrReq, err := req.JsonRpcRequest(ctx)
	if err != nil {
		return "", err
	}

	jrReq.RLock()
	defer jrReq.RUnlock()

	if len(jrReq.Params) == 0 {
		return "", fmt.Errorf("no parameters found in request")
	}

	// Filter ID is typically the first parameter for filter operations
	if filterId, ok := jrReq.Params[0].(string); ok {
		if filterIDPattern.MatchString(filterId) {
			return filterId, nil
		}
		return "", fmt.Errorf("invalid filter ID format: %s", filterId)
	}

	return "", fmt.Errorf("filter ID not found in parameters")
}

// ExtractFilterIdFromResponse extracts filter ID from response for filter creation methods
func (fm *FilterManager) ExtractFilterIdFromResponse(ctx context.Context, resp *common.NormalizedResponse) (string, error) {
	jrResp, err := resp.JsonRpcResponse(ctx)
	if err != nil {
		return "", err
	}

	// Get the result as a string (filter ID)
	resultStr, err := jrResp.PeekStringByPath(ctx)
	if err != nil {
		return "", err
	}

	if filterIDPattern.MatchString(resultStr) {
		return resultStr, nil
	}

	return "", fmt.Errorf("invalid filter ID format in response: %s", resultStr)
}

func (fm *FilterManager) getFilterKey(filterId, networkId, projectId string) string {
	return fmt.Sprintf("filter:%s:%s:%s", projectId, networkId, filterId)
}
