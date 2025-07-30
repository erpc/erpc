package data

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/telemetry"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

// subscriberChannel wraps a channel with metadata to prevent double-close
type subscriberChannel struct {
	ch     chan int64
	closed bool
	mu     sync.Mutex
}

// RedisPubSubManager is a centralized singleton for managing Redis pubsub subscriptions
// It uses a copy-on-write pattern for subscriber management to avoid race conditions.
// When subscribers are added or removed, a new slice is created rather than modifying
// the existing one, allowing concurrent readers to safely iterate without locks.
type RedisPubSubManager struct {
	client         *redis.Client
	logger         *zerolog.Logger
	ctx            context.Context
	cancel         context.CancelFunc
	pubsub         *redis.PubSub
	subscribers    sync.Map // map[string][]*subscriberChannel - uses copy-on-write
	mu             sync.RWMutex // protects started/stopped state only
	started        bool
	stopped        bool
	pollInterval   time.Duration
	redisConnector *RedisConnector
}

// NewRedisPubSubManager creates a new centralized pubsub manager
func NewRedisPubSubManager(
	ctx context.Context,
	logger *zerolog.Logger,
	client *redis.Client,
	redisConnector *RedisConnector,
) *RedisPubSubManager {
	ctx, cancel := context.WithCancel(ctx)
	lg := logger.With().Str("component", "redisPubSubManager").Logger()
	return &RedisPubSubManager{
		client:         client,
		logger:         &lg,
		ctx:            ctx,
		cancel:         cancel,
		pollInterval:   5 * time.Minute,
		redisConnector: redisConnector,
	}
}

// Start initializes the pubsub subscription and starts listening
func (m *RedisPubSubManager) Start() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return nil
	}

	// Subscribe to all counter updates using pattern
	m.pubsub = m.client.PSubscribe(m.ctx, "counter:*")

	// Wait for subscription confirmation
	_, err := m.pubsub.Receive(m.ctx)
	if err != nil {
		return fmt.Errorf("failed to subscribe to counter:* pattern: %w", err)
	}

	m.started = true
	m.logger.Info().Msg("started Redis pubsub manager with pattern subscription")

	// Start the main message handling loop
	go m.messageLoop()

	// Start the polling loop
	go m.pollingLoop()

	return nil
}

// Stop gracefully shuts down the pubsub manager
func (m *RedisPubSubManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.started {
		return nil
	}

	m.cancel()
	m.started = false

	if m.pubsub != nil {
		if err := m.pubsub.Close(); err != nil {
			m.logger.Warn().Err(err).Msg("error closing pubsub connection")
			return err
		}
	}

	m.stopped = true

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

	m.logger.Info().Msg("stopped Redis pubsub manager")
	return nil
}

// Subscribe adds a subscription for a specific key
func (m *RedisPubSubManager) Subscribe(key string) (<-chan int64, func(), error) {
	m.mu.RLock()
	if m.stopped {
		m.mu.RUnlock()
		return nil, nil, fmt.Errorf("pubsub manager is stopped")
	}
	m.mu.RUnlock()

	// Ensure manager is started
	if !m.started {
		if err := m.Start(); err != nil {
			return nil, nil, fmt.Errorf("failed to start pubsub manager: %w", err)
		}
	}

	sc := &subscriberChannel{
		ch: make(chan int64, 1),
	}

	// Add the channel to subscribers
	m.addSubscriber(key, sc)

	// Create a context for the initial value goroutine
	initCtx, initCancel := context.WithCancel(m.ctx)

	// Get initial value
	go func() {
		defer initCancel()
		if val, err := m.getCurrentValue(initCtx, key); err == nil && val > 0 {
			sc.mu.Lock()
			if !sc.closed {
				select {
				case sc.ch <- val:
				case <-initCtx.Done():
				}
			}
			sc.mu.Unlock()
		}
	}()

	// Return cleanup function
	cleanup := func() {
		initCancel() // Cancel the initial value goroutine if still running
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

// messageLoop handles incoming pubsub messages
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
	}()

	ch := m.pubsub.Channel()
	for {
		select {
		case <-m.ctx.Done():
			return

		case msg, ok := <-ch:
			if !ok {
				m.logger.Warn().Msg("pubsub channel closed")
				return
			}

			if msg != nil && strings.HasPrefix(msg.Channel, "counter:") {
				m.logger.Debug().Str("channel", msg.Channel).Str("payload", msg.Payload).Msg("received pubsub message")
				key := strings.TrimPrefix(msg.Channel, "counter:")
				if val, err := strconv.ParseInt(msg.Payload, 10, 64); err == nil {
					m.notifySubscribers(key, val)
				} else {
					m.logger.Warn().
						Str("key", key).
						Str("payload", msg.Payload).
						Msg("failed to parse counter value from pubsub message")
				}
			} else {
				m.logger.Trace().Str("channel", msg.Channel).Str("payload", msg.Payload).Msg("received pubsub message with unknown prefix")
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
		case <-m.ctx.Done():
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
		if val, err := m.getCurrentValue(m.ctx, key); err == nil && val > 0 {
			m.notifySubscribers(key, val)
		} else if err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			m.logger.Warn().Err(err).Str("key", key).Msg("failed to poll counter value")
		}
	}
}

// getCurrentValue fetches the current value of a counter
func (m *RedisPubSubManager) getCurrentValue(ctx context.Context, key string) (int64, error) {
	val, err := m.redisConnector.Get(ctx, ConnectorMainIndex, key, "value")
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return 0, nil
		}
		return 0, err
	}

	value, err := strconv.ParseInt(string(val), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse counter value: %w", err)
	}

	return value, nil
}

// addSubscriber adds a channel to the subscribers for a key
// This method is thread-safe and uses copy-on-write to avoid race conditions.
// Concurrent readers (like notifySubscribers) will continue to use the old slice.
func (m *RedisPubSubManager) addSubscriber(key string, sc *subscriberChannel) {
	// Load existing subscribers or create empty slice
	value, _ := m.subscribers.LoadOrStore(key, []*subscriberChannel{})
	existing := value.([]*subscriberChannel)
	
	// Create a new slice with the new subscriber (copy-on-write)
	newChannels := make([]*subscriberChannel, len(existing)+1)
	copy(newChannels, existing)
	newChannels[len(existing)] = sc
	
	// Store the new slice atomically
	m.subscribers.Store(key, newChannels)

	m.logger.Debug().Str("key", key).Int("subscribers", len(newChannels)).Msg("added subscriber")
}

// removeSubscriber removes a channel from the subscribers for a key
// This method is thread-safe and uses copy-on-write to avoid race conditions.
func (m *RedisPubSubManager) removeSubscriber(key string, sc *subscriberChannel) {
	value, ok := m.subscribers.Load(key)
	if !ok {
		return
	}

	existing := value.([]*subscriberChannel)
	
	// Create a new slice without the subscriber (copy-on-write)
	newChannels := make([]*subscriberChannel, 0, len(existing)-1)
	for _, c := range existing {
		if c != sc {
			newChannels = append(newChannels, c)
		}
	}

	if len(newChannels) == 0 {
		m.subscribers.Delete(key)
		m.logger.Debug().Str("key", key).Msg("removed last subscriber")
	} else {
		m.subscribers.Store(key, newChannels)
		m.logger.Debug().Str("key", key).Int("subscribers", len(newChannels)).Msg("removed subscriber")
	}
}

// notifySubscribers sends a value to all subscribers of a key
func (m *RedisPubSubManager) notifySubscribers(key string, value int64) {
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