package data

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

// subscriberChannel wraps a channel with metadata to prevent double-close
type subscriberChannel struct {
	ch     chan CounterInt64State
	closed bool
	mu     sync.Mutex
}

// RedisPubSubManager is a self-healing manager for Redis pubsub subscriptions.
// Key design principles:
// - Transparent reconnection - subscribers never see disconnections
// - Self-healing message loop that reconnects automatically
// - Copy-on-write pattern for subscriber management
// - Zero disruption to active subscribers during reconnection
type RedisPubSubManager struct {
	connector    *RedisConnector // Reference to get current client
	logger       *zerolog.Logger
	appCtx       context.Context
	cancel       context.CancelFunc
	pubsub       *redis.PubSub
	subscribers  sync.Map     // map[string][]*subscriberChannel - NEVER cleared
	mu           sync.RWMutex // Protects pubsub operations
	running      atomic.Bool
	pollInterval time.Duration
}

// NewRedisPubSubManager creates a new centralized pubsub manager
func NewRedisPubSubManager(
	appCtx context.Context,
	logger *zerolog.Logger,
	connector *RedisConnector,
) *RedisPubSubManager {
	lg := logger.With().Str("component", "redisPubSubManager").Logger()

	m := &RedisPubSubManager{
		connector:    connector,
		logger:       &lg,
		appCtx:       appCtx,
		pollInterval: 5 * time.Minute,
	}

	// Start immediately - if it fails, Subscribe will retry
	if err := m.start(); err != nil {
		m.logger.Error().Err(err).Msg("failed to start pubsub manager on creation")
		// Don't fail construction - let Subscribe handle retries
	}

	// Stop the manager when the context is done
	go func() {
		<-appCtx.Done()
		m.stop()
	}()

	return m
}

// start initializes the pubsub subscription and starts listening
// This is now an internal method - external callers just use Subscribe
func (m *RedisPubSubManager) start() error {
	// Check if already running
	if !m.running.CompareAndSwap(false, true) {
		return nil // Already running
	}

	// Initial connection attempt
	if err := m.reconnectPubSub(); err != nil {
		m.running.Store(false)
		return err
	}

	// Start the goroutines
	go m.messageLoop()
	go m.pollingLoop()

	return nil
}

// stop gracefully shuts down the pubsub manager
// This is called automatically when the app context is closing
func (m *RedisPubSubManager) stop() {
	// Ensure we only stop once
	if !m.running.CompareAndSwap(true, false) {
		return // Already stopped
	}

	// Close pubsub connection
	if m.pubsub != nil {
		if err := m.pubsub.Close(); err != nil {
			m.logger.Warn().Err(err).Msg("error closing pubsub connection")
		}
	}

	// Close all subscriber channels
	m.subscribers.Range(func(key, value interface{}) bool {
		channels := value.([]*subscriberChannel)
		for _, sc := range channels {
			sc.mu.Lock()
			if !sc.closed {
				close(sc.ch)
				sc.closed = true
			}
			sc.mu.Unlock()
		}
		return true
	})

	m.logger.Info().Msg("stopped redis pubsub manager")
}

// reconnectPubSub safely reconnects the pubsub subscription without disrupting subscribers
func (m *RedisPubSubManager) reconnectPubSub() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Close old pubsub if exists
	if m.pubsub != nil {
		if err := m.pubsub.Close(); err != nil {
			m.logger.Debug().Err(err).Msg("error closing old pubsub connection")
		}
		m.pubsub = nil
	}

	// Ensure main client is available and healthy
	if m.connector.client == nil {
		return fmt.Errorf("redis client not available")
	}

	// Test main client health before creating pubsub
	pingCtx, cancel := context.WithTimeout(m.appCtx, 3*time.Second)
	err := m.connector.client.Ping(pingCtx).Err()
	cancel()

	if err != nil {
		m.logger.Debug().Err(err).Msg("main redis client unhealthy, deferring pubsub reconnection")
		return fmt.Errorf("main redis client unhealthy: %w", err)
	}

	// Create new pubsub subscription
	m.pubsub = m.connector.client.PSubscribe(m.appCtx, "counter:*")

	// Wait for subscription confirmation with timeout
	confirmCtx, cancelConfirm := context.WithTimeout(m.appCtx, 10*time.Second)
	_, err = m.pubsub.Receive(confirmCtx)
	cancelConfirm()

	if err != nil {
		// Clean up failed pubsub
		if m.pubsub != nil {
			_ = m.pubsub.Close()
			m.pubsub = nil
		}
		return fmt.Errorf("failed to subscribe to counter:* pattern: %w", err)
	}

	m.logger.Info().Msg("pubsub reconnected successfully")

	// Force poll to ensure subscribers get latest values (but don't block)
	go m.pollAllKeys()

	return nil
}

// Subscribe adds a subscription for a specific key
func (m *RedisPubSubManager) Subscribe(key string) (<-chan CounterInt64State, func(), error) {
	// Check if context is done (manager is stopped)
	if m.appCtx.Err() != nil {
		return nil, nil, fmt.Errorf("pubsub manager is stopped")
	}

	// Ensure manager is running
	if !m.running.Load() {
		if err := m.start(); err != nil {
			return nil, nil, fmt.Errorf("failed to start pubsub manager: %w", err)
		}
	}

	sc := &subscriberChannel{
		ch: make(chan CounterInt64State, 1),
	}

	// Add the channel to subscribers
	m.addSubscriber(key, sc)

	// Get initial value in background
	go func() {
		if val, ok, err := m.getCurrentValue(m.appCtx, key); err == nil && ok {
			sc.mu.Lock()
			if !sc.closed {
				select {
				case sc.ch <- val:
				case <-m.appCtx.Done():
				}
			}
			sc.mu.Unlock()
		}
	}()

	// Return cleanup function
	cleanup := func() {
		m.removeSubscriber(key, sc)
		sc.mu.Lock()
		if !sc.closed {
			close(sc.ch)
			sc.closed = true
		}
		sc.mu.Unlock()
	}

	return sc.ch, cleanup, nil
}

// messageLoop handles incoming pubsub messages with self-healing reconnection
func (m *RedisPubSubManager) messageLoop() {
	defer func() {
		if r := recover(); r != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"redis-pubsub-message-loop",
				"redis-pubsub-manager",
				common.ErrorFingerprint(r),
			).Inc()
			m.logger.Error().
				Interface("panic", r).
				Msg("unexpected panic in Redis pubsub message loop")
		}
		// Mark as not running on exit
		m.running.Store(false)
	}()

	for {
		if err := m.runMessageLoop(); err != nil {
			if m.appCtx.Err() != nil {
				return // Manager is stopping
			}

			// Connection lost - use more graceful reconnection with jitter
			m.logger.Warn().Err(err).Msg("pubsub connection lost, attempting graceful reconnection...")

			retryDelay := time.Second
			maxRetryDelay := 30 * time.Second

			for {
				if m.appCtx.Err() != nil {
					return // Manager is stopping
				}

				// Add jitter to prevent thundering herd
				jitter := time.Duration(rand.Int63n(int64(retryDelay / 2))) // #nosec G404
				actualDelay := retryDelay + jitter

				m.logger.Debug().Dur("delay", actualDelay).Msg("waiting before pubsub reconnection attempt")
				select {
				case <-time.After(actualDelay):
				case <-m.appCtx.Done():
					return
				}

				if err := m.reconnectPubSub(); err != nil {
					m.logger.Error().Err(err).Dur("nextRetryDelay", retryDelay*2).Msg("failed to reconnect pubsub, will retry...")
					// Exponential backoff with max delay
					if retryDelay < maxRetryDelay {
						retryDelay *= 2
					}
					continue
				}

				m.logger.Info().Msg("pubsub reconnected successfully in message loop")
				break
			}
		}
	}
}

// runMessageLoop runs the actual message processing loop
func (m *RedisPubSubManager) runMessageLoop() error {
	m.mu.RLock()
	if m.pubsub == nil {
		m.mu.RUnlock()
		return fmt.Errorf("pubsub not initialized")
	}
	ch := m.pubsub.Channel()
	m.mu.RUnlock()

	for {
		select {
		case <-m.appCtx.Done():
			return nil

		case msg, ok := <-ch:
			if !ok {
				return fmt.Errorf("pubsub channel closed")
			}

			if msg != nil && strings.HasPrefix(msg.Channel, "counter:") {
				key := strings.TrimPrefix(msg.Channel, "counter:")
				var st CounterInt64State
				if err := common.SonicCfg.Unmarshal([]byte(msg.Payload), &st); err == nil && st.UpdatedAt > 0 {
					m.logger.Debug().
						Str("key", key).
						Int64("value", st.Value).
						Int64("updatedAt", st.UpdatedAt).
						Str("updatedBy", st.UpdatedBy).
						Msg("received counter update via pubsub")
					m.notifySubscribers(key, st)
					continue
				}
				m.logger.Debug().
					Str("key", key).
					Str("payload", msg.Payload).
					Msg("failed to parse counter state from pubsub message")
			}
		}
	}
}

// pollingLoop periodically polls all subscribed keys
func (m *RedisPubSubManager) pollingLoop() {
	defer func() {
		if r := recover(); r != nil {
			telemetry.MetricUnexpectedPanicTotal.WithLabelValues(
				"redis-pubsub-polling-loop",
				"redis-pubsub-manager",
				common.ErrorFingerprint(r),
			).Inc()
			m.logger.Error().
				Interface("panic", r).
				Msg("unexpected panic in Redis pubsub polling loop")
		}
	}()

	ticker := time.NewTicker(m.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-m.appCtx.Done():
			return

		case <-ticker.C:
			m.pollAllKeys()
		}
	}
}

// pollAllKeys polls current values for all subscribed keys
func (m *RedisPubSubManager) pollAllKeys() {
	keys := make([]string, 0)
	m.subscribers.Range(func(key, _ interface{}) bool {
		keys = append(keys, key.(string))
		return true
	})

	if len(keys) == 0 {
		return
	}

	m.logger.Debug().Int("keyCount", len(keys)).Msg("polling counter values")

	for _, key := range keys {
		if val, ok, err := m.getCurrentValue(m.appCtx, key); err == nil && ok {
			m.notifySubscribers(key, val)
		}
	}
}

// getCurrentValue fetches the current value of a counter
func (m *RedisPubSubManager) getCurrentValue(ctx context.Context, key string) (CounterInt64State, bool, error) {
	val, err := m.connector.Get(ctx, ConnectorMainIndex, key, "value", nil)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return CounterInt64State{}, false, nil
		}
		return CounterInt64State{}, false, err
	}

	var st CounterInt64State
	if err := common.SonicCfg.Unmarshal(val, &st); err != nil {
		// No backward compatibility: treat parse errors as missing
		return CounterInt64State{}, false, nil
	}
	if st.UpdatedAt <= 0 {
		return CounterInt64State{}, false, nil
	}
	return st, true, nil
}

// addSubscriber adds a channel to the subscribers for a key
// Uses copy-on-write pattern for thread-safety
func (m *RedisPubSubManager) addSubscriber(key string, sc *subscriberChannel) {
	value, _ := m.subscribers.LoadOrStore(key, []*subscriberChannel{})
	existing := value.([]*subscriberChannel)

	// Copy-on-write
	newChannels := make([]*subscriberChannel, len(existing)+1)
	copy(newChannels, existing)
	newChannels[len(existing)] = sc

	m.subscribers.Store(key, newChannels)
}

// removeSubscriber removes a channel from the subscribers for a key
// Uses copy-on-write pattern for thread-safety
func (m *RedisPubSubManager) removeSubscriber(key string, sc *subscriberChannel) {
	value, ok := m.subscribers.Load(key)
	if !ok {
		return
	}

	existing := value.([]*subscriberChannel)

	// Copy-on-write
	newChannels := make([]*subscriberChannel, 0, len(existing)-1)
	for _, c := range existing {
		if c != sc {
			newChannels = append(newChannels, c)
		}
	}

	if len(newChannels) == 0 {
		m.subscribers.Delete(key)
	} else {
		m.subscribers.Store(key, newChannels)
	}
}

// notifySubscribers sends a value to all subscribers of a key
func (m *RedisPubSubManager) notifySubscribers(key string, value CounterInt64State) {
	subsValue, ok := m.subscribers.Load(key)
	if !ok {
		return
	}

	channels := subsValue.([]*subscriberChannel)
	for _, sc := range channels {
		sc.mu.Lock()
		if !sc.closed {
			select {
			case sc.ch <- value:
			default:
				// Channel is full, skip
			}
		}
		sc.mu.Unlock()
	}
}
