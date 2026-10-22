package dal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
)

// RedisCacheProvider implements CacheProvider using Redis Pub/Sub and Hash buffers.
// ARCHITECTURE: This concrete implementation is strictly confined to the infrastructure/repository
// layer. The Service layer only ever interacts with Repository interfaces (or CacheProvider for
// background flushers), remaining completely decoupled from go-redis.
type RedisCacheProvider struct {
	client  *redis.Client
	options *redis.Options
	ctx     context.Context
	cancel  context.CancelFunc

	// handlers maps an entity name to its invalidation handler.
	handlers          map[string]func(string)
	flushListHandlers map[string]func()
	flushItemHandlers map[string]func()
	bumpEpochHandlers map[string]func()
	pubsubs           []*redis.PubSub // track active subscriptions for clean teardown
	mu                sync.RWMutex

	isClosed  bool
	telemetry TelemetryProvider
}

// NewRedisCacheProvider creates a new instance with the given Redis config.
func NewRedisCacheProvider(addr, password string, db int, telemetry TelemetryProvider) *RedisCacheProvider {
	ctx, cancel := context.WithCancel(context.Background())

	// Fallback for tests or components not using telemetry
	if telemetry == nil {
		telemetry = NoopTelemetryProvider{}
	}

	return &RedisCacheProvider{
		options: &redis.Options{
			Addr:     addr,
			Password: password,
			DB:       db,
			OnConnect: func(ctx context.Context, cn *redis.Conn) error {
				return nil
			},
		},
		ctx:               ctx,
		cancel:            cancel,
		handlers:          make(map[string]func(string)),
		flushListHandlers: make(map[string]func()),
		flushItemHandlers: make(map[string]func()),
		bumpEpochHandlers: make(map[string]func()),
		telemetry:         telemetry,
	}
}

// Connect establishes a Redis connection with exponential backoff.
func (p *RedisCacheProvider) Connect() error {
	var err error
	backoff := 1 * time.Second
	maxBackoff := 30 * time.Second
	maxRetries := 5

	for i := 0; i < maxRetries; i++ {
		log.Infof("Pinging redis for connection: [%s]", p.options.Addr)
		p.client = redis.NewClient(p.options)
		_, err = p.client.Ping(p.ctx).Result()
		if err == nil {
			log.Infof("Connected to redis: [%s]", p.options.Addr)
			return nil
		}

		log.Errorf("Redis connection failed: %v. Retrying in %v...\n", err, backoff)
		time.Sleep(backoff)
		backoff = time.Duration(float64(backoff) * 1.5)
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	p.telemetry.IncCachePubSubError(p.options.Addr)
	log.Warnln("Redis connection permanently failed. Cache invalidation may be unreliable.")
	return fmt.Errorf("failed to connect to Redis after %d retries: %w", maxRetries, err)
}

// publishAsync handles fire-and-forget Pub/Sub broadcasting with context-aware retry logic.
func (p *RedisCacheProvider) publishAsync(channel, payload, logLabel string) error {
	p.mu.RLock()
	closed := p.isClosed
	p.mu.RUnlock()

	if closed {
		p.telemetry.IncCachePubSubError(p.options.Addr)
		return errors.New("cache provider is closed")
	}

	go func() {
		const maxRetries = 3
		const delay = 5 * time.Second

		for i := 0; i < maxRetries; i++ {
			// Check context before attempting publish
			if p.ctx.Err() != nil {
				return
			}

			err := p.client.Publish(p.ctx, channel, payload).Err()
			if err == nil {
				p.telemetry.IncCachePubSubPublish(p.options.Addr)
				log.Debugf("Published %s: %s -> %s\n", logLabel, channel, payload)
				return
			}

			log.Errorf("Failed to publish %s (%s -> %s): %v. Retrying in %v...\n", logLabel, channel, payload, err, delay)

			// Context-aware sleep so Close() cancels pending retry goroutines immediately
			select {
			case <-p.ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		p.telemetry.IncCachePubSubError(p.options.Addr)
		log.Errorf("Failed to publish %s (%s -> %s) after %d retries\n", logLabel, channel, payload, maxRetries)
	}()

	return nil
}

// InvalidateCache publishes an invalidation message asynchronously (fire and forget).
func (p *RedisCacheProvider) InvalidateCache(entityName, cacheKey string) error {
	channel := fmt.Sprintf("cache_invalidation_%s", entityName)
	return p.publishAsync(channel, cacheKey, "cache_invalidation")
}

// FlushListCache publishes a flush list message asynchronously (fire and forget).
func (p *RedisCacheProvider) FlushListCache(entityName string) error {
	channel := fmt.Sprintf("cache_flush_list_%s", entityName)
	return p.publishAsync(channel, entityName, "cache_flush_list")
}

// FlushItemCache publishes a flush item message asynchronously (fire and forget).
func (p *RedisCacheProvider) FlushItemCache(entityName string) error {
	channel := fmt.Sprintf("cache_flush_item_%s", entityName)
	return p.publishAsync(channel, entityName, "cache_flush_item")
}

// BumpEpoch publishes an epoch bump message asynchronously (fire and forget).
func (p *RedisCacheProvider) BumpEpoch(entityName string) error {
	channel := fmt.Sprintf("cache_bump_epoch_%s", entityName)
	return p.publishAsync(channel, entityName, "cache_bump_epoch")
}

// subscribeChannelLocked creates the Redis PubSub subscription and awaits the initial
// SUBSCRIBE confirmation synchronously.
//
// ARCHITECTURE WHY: Calling pubsub.Receive(p.ctx) before spawning the listener goroutine
// guarantees the Redis server has registered the subscription before OnCache* returns.
// This eliminates race conditions where immediate writes/publishes drop messages into the void,
// without needing arbitrary time.Sleep() calls.
// Caller MUST hold p.mu.Lock().
func (p *RedisCacheProvider) subscribeChannelLocked(channel string) <-chan *redis.Message {
	pubsub := p.client.Subscribe(p.ctx, channel)
	p.pubsubs = append(p.pubsubs, pubsub)

	// Block until Redis confirms the subscription (or context is cancelled)
	if _, err := pubsub.Receive(p.ctx); err != nil {
		log.Errorf("Failed to receive Redis subscription confirmation for channel %s: %v\n", channel, err)
	}

	return pubsub.Channel()
}

// OnCacheInvalidated registers a handler for a specific entity.
func (p *RedisCacheProvider) OnCacheInvalidated(entityName string, handler func(string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isClosed {
		return
	}

	if _, exists := p.handlers[entityName]; !exists {
		p.handlers[entityName] = handler
		channel := fmt.Sprintf("cache_invalidation_%s", entityName)
		msgChan := p.subscribeChannelLocked(channel)
		go p.listenForInvalidations(entityName, msgChan)
	}
}

// OnCacheFlushList registers a handler for an entity's flush_list event.
func (p *RedisCacheProvider) OnCacheFlushList(entityName string, handler func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isClosed {
		return
	}

	if _, exists := p.flushListHandlers[entityName]; !exists {
		p.flushListHandlers[entityName] = handler
		channel := fmt.Sprintf("cache_flush_list_%s", entityName)
		msgChan := p.subscribeChannelLocked(channel)
		go p.listenForFlushList(entityName, msgChan)
	}
}

// OnCacheFlushItem registers a handler for an entity's flush_item event.
func (p *RedisCacheProvider) OnCacheFlushItem(entityName string, handler func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isClosed {
		return
	}

	if _, exists := p.flushItemHandlers[entityName]; !exists {
		p.flushItemHandlers[entityName] = handler
		channel := fmt.Sprintf("cache_flush_item_%s", entityName)
		msgChan := p.subscribeChannelLocked(channel)
		go p.listenForFlushItem(entityName, msgChan)
	}
}

// OnBumpEpoch registers a handler for an entity's bump_epoch event.
func (p *RedisCacheProvider) OnBumpEpoch(entityName string, handler func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.isClosed {
		return
	}

	if _, exists := p.bumpEpochHandlers[entityName]; !exists {
		p.bumpEpochHandlers[entityName] = handler
		channel := fmt.Sprintf("cache_bump_epoch_%s", entityName)
		msgChan := p.subscribeChannelLocked(channel)
		go p.listenForBumpEpoch(entityName, msgChan)
	}
}

// listenForInvalidations processes incoming single-key invalidation messages for an entity.
func (p *RedisCacheProvider) listenForInvalidations(entityName string, msgChan <-chan *redis.Message) {
	log.Debugf("Listening for cache invalidation messages for %s\n", entityName)
	for msg := range msgChan {
		log.Debugf("Got cache invalidation message for %s\n", entityName)
		p.mu.RLock()
		handler, exists := p.handlers[entityName]
		p.mu.RUnlock()
		if exists {
			handler(msg.Payload)
			p.telemetry.IncCachePubSubReceive(p.options.Addr)
		} else {
			log.Warnf("No handler registered for entity: %s\n", entityName)
		}
	}
}

// listenForFlushList processes incoming flush_list messages for an entity.
func (p *RedisCacheProvider) listenForFlushList(entityName string, msgChan <-chan *redis.Message) {
	log.Debugf("Listening for cache_flush_list messages for %s\n", entityName)
	for range msgChan {
		log.Debugf("Got cache_flush_list message for %s\n", entityName)
		p.mu.RLock()
		handler, exists := p.flushListHandlers[entityName]
		p.mu.RUnlock()
		if exists {
			handler()
			p.telemetry.IncCachePubSubReceive(p.options.Addr)
		} else {
			log.Warnf("No cache_flush_list handler registered for entity: %s\n", entityName)
		}
	}
}

// listenForFlushItem processes incoming cache_flush_item messages for an entity.
func (p *RedisCacheProvider) listenForFlushItem(entityName string, msgChan <-chan *redis.Message) {
	log.Debugf("Listening for cache_flush_item messages for %s\n", entityName)
	for range msgChan {
		log.Debugf("Got cache_flush_item message for %s\n", entityName)
		p.mu.RLock()
		handler, exists := p.flushItemHandlers[entityName]
		p.mu.RUnlock()
		if exists {
			handler()
			p.telemetry.IncCachePubSubReceive(p.options.Addr)
		} else {
			log.Warnf("No cache_flush_item handler registered for entity: %s\n", entityName)
		}
	}
}

// listenForBumpEpoch processes incoming cache_bump_epoch messages for an entity.
func (p *RedisCacheProvider) listenForBumpEpoch(entityName string, msgChan <-chan *redis.Message) {
	log.Debugf("Listening for cache_bump_epoch messages for %s\n", entityName)
	for range msgChan {
		log.Debugf("Got cache_bump_epoch message for %s\n", entityName)
		p.mu.RLock()
		handler, exists := p.bumpEpochHandlers[entityName]
		p.mu.RUnlock()
		if exists {
			handler()
			p.telemetry.IncCachePubSubReceive(p.options.Addr)
		} else {
			log.Warnf("No cache_bump_epoch handler registered for entity: %s\n", entityName)
		}
	}
}

// Close shuts down the provider and cleanly terminates all Pub/Sub subscriptions.
func (p *RedisCacheProvider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isClosed {
		return
	}
	log.Infoln("Closing RedisCacheProvider...")
	p.isClosed = true
	p.cancel()

	// Gracefully close all Pub/Sub subscriptions FIRST so msgChan loops exit cleanly
	for _, ps := range p.pubsubs {
		_ = ps.Close()
	}
	p.pubsubs = nil

	if p.client != nil {
		_ = p.client.Close()
	}
}

// --- Buffer Operations for Write-Behind Cache ---

func (p *RedisCacheProvider) HIncrBy(ctx context.Context, key, field string, incr int64) error {
	return p.client.HIncrBy(ctx, key, field, incr).Err()
}

func (p *RedisCacheProvider) HScan(ctx context.Context, key string, cursor uint64, match string, count int64) ([]string, uint64, error) {
	return p.client.HScan(ctx, key, cursor, match, count).Result()
}

func (p *RedisCacheProvider) Rename(ctx context.Context, oldKey, newKey string) error {
	return p.client.Rename(ctx, oldKey, newKey).Err()
}

func (p *RedisCacheProvider) Del(ctx context.Context, keys ...string) error {
	return p.client.Del(ctx, keys...).Err()
}

func (p *RedisCacheProvider) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error) {
	return p.client.SetNX(ctx, key, value, expiration).Result()
}
