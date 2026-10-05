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

// RedisCacheProvider implements CacheProvider using Redis Pub/Sub.
type RedisCacheProvider struct {
	client  *redis.Client
	options *redis.Options
	ctx     context.Context
	cancel  context.CancelFunc

	// handlers maps an entity name to its invalidation handler.
	handlers          map[string]func(string)
	flushListHandlers map[string]func()
	flushItemHandlers map[string]func()
	pubsubs           []*redis.PubSub // track active subscriptions
	mu                sync.RWMutex

	isClosed          bool
	telemetry         TelemetryProvider
	bumpEpochHandlers map[string]func()
}

// NewRedisCacheProvider creates a new instance with the given Redis config.
func NewRedisCacheProvider(addr, password string, db int, telemetry TelemetryProvider) *RedisCacheProvider {
	ctx, cancel := context.WithCancel(context.Background())

	// Fallback for tests or components not using telemetry
	if telemetry == nil {
		telemetry = NoopTelemetryProvider{}
	}

	provider := &RedisCacheProvider{
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

	return provider
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

// InvalidateCache publishes an invalidation message asynchronously (fire and forget).
func (p *RedisCacheProvider) InvalidateCache(entityName, cacheKey string) error {
	if p.isClosed {
		p.telemetry.IncCachePubSubError(p.options.Addr)
		return errors.New("cache provider is closed")
	}

	// Execute publish asynchronously.
	go func() {
		channel := fmt.Sprintf("cache_invalidation_%s", entityName)
		message := cacheKey
		maxRetries := 3
		delay := 5 * time.Second

		for i := 0; i < maxRetries; i++ {
			err := p.client.Publish(p.ctx, channel, message).Err()
			if err == nil {
				p.telemetry.IncCachePubSubPublish(p.options.Addr)
				log.Debugf("Published cache invalidation: %s -> %s\n", entityName, cacheKey)
				return
			}

			log.Errorf("Failed to publish cache invalidation: %s -> %s. Retrying in %v...\n", entityName, cacheKey, delay)
			time.Sleep(delay)
		}

		p.telemetry.IncCachePubSubError(p.options.Addr)
		log.Errorf("Failed to publish cache invalidation: %s -> %s after %d retries\n", entityName, cacheKey, maxRetries)
	}()
	return nil // Return immediately.
}

// FlushListCache publishes an flush list message asynchronously (fire and forget).
func (p *RedisCacheProvider) FlushListCache(entityName string) error {
	if p.isClosed {
		p.telemetry.IncCachePubSubError(p.options.Addr)
		return errors.New("cache provider is closed")
	}

	// Execute publish asynchronously.
	go func() {
		channel := fmt.Sprintf("cache_flush_list_%s", entityName)
		message := entityName
		maxRetries := 3
		delay := 5 * time.Second

		for i := 0; i < maxRetries; i++ {
			err := p.client.Publish(p.ctx, channel, message).Err()
			if err == nil {
				p.telemetry.IncCachePubSubPublish(p.options.Addr)
				log.Debugf("Published cache_flush_list: %s \n", entityName)
				return
			}

			log.Errorf("Failed to publish cache_flush_list: %s. Retrying in %v...\n", entityName, delay)
			time.Sleep(delay)
		}

		p.telemetry.IncCachePubSubError(p.options.Addr)
		log.Errorf("Failed to publish cache_flush_list: %s after %d retries\n", entityName, maxRetries)
	}()
	return nil // Return immediately.
}

// FlushItemCache publishes a flush item message asynchronously (fire and forget).
func (p *RedisCacheProvider) FlushItemCache(entityName string) error {
	if p.isClosed {
		p.telemetry.IncCachePubSubError(p.options.Addr)
		return errors.New("cache provider is closed")
	}

	// Execute publish asynchronously.
	go func() {
		channel := fmt.Sprintf("cache_flush_item_%s", entityName)
		message := entityName
		maxRetries := 3
		delay := 5 * time.Second

		for i := 0; i < maxRetries; i++ {
			err := p.client.Publish(p.ctx, channel, message).Err()
			if err == nil {
				p.telemetry.IncCachePubSubPublish(p.options.Addr)
				log.Debugf("Published cache_flush_item: %s \n", entityName)
				return
			}

			log.Errorf("Failed to publish cache_flush_item: %s. Retrying in %v...\n", entityName, delay)
			time.Sleep(delay)
		}

		p.telemetry.IncCachePubSubError(p.options.Addr)
		log.Errorf("Failed to publish cache_flush_item: %s after %d retries\n", entityName, maxRetries)
	}()
	return nil // Return immediately.
}

// OnCacheInvalidated registers a handler for a specific entity.
func (p *RedisCacheProvider) OnCacheInvalidated(entityName string, handler func(string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.handlers[entityName]
	if !exists {
		p.handlers[entityName] = handler
		go p.listenForInvalidations(entityName)
	}
}

func (p *RedisCacheProvider) OnCacheFlushList(entityName string, handler func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.flushListHandlers[entityName]
	if !exists {
		p.flushListHandlers[entityName] = handler
		go p.listenForFlushList(entityName)
	}
}

// listenForInvalidations subscribes to all entity channels using a pattern.
func (p *RedisCacheProvider) listenForInvalidations(entityName string) {
	time.Sleep(2 * time.Second)
	channel := fmt.Sprintf("cache_invalidation_%s", entityName)
	pubsub := p.client.Subscribe(p.ctx, channel)

	// 👈 Track the pubsub so we can close it later
	p.mu.Lock()
	p.pubsubs = append(p.pubsubs, pubsub)
	p.mu.Unlock()

	defer pubsub.Close()

	// We got our pubsub created so lets process incoming messages in their own
	log.Debugf("Listening for cache invalidation messages for %s\n", entityName)
	for msg := range pubsub.Channel() {
		log.Debugf("Got cache invalidation message for %s\n", entityName)
		// Extract entity name from the channel name.
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

// listenForFlushList subscribes to an entity's flush_list channel
func (p *RedisCacheProvider) listenForFlushList(entityName string) {
	time.Sleep(2 * time.Second)
	channel := fmt.Sprintf("cache_flush_list_%s", entityName)
	pubsub := p.client.Subscribe(p.ctx, channel)

	// 👈 Track the pubsub so we can close it later
	p.mu.Lock()
	p.pubsubs = append(p.pubsubs, pubsub)
	p.mu.Unlock()

	defer pubsub.Close()

	// We got our pubsub created so lets process incoming messages in their own
	log.Debugf("Listening for cache_flush_list messages for %s\n", entityName)
	for range pubsub.Channel() {
		log.Debugf("Got cache_flush_list message for %s\n", entityName)
		// Extract entity name from the channel name.
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

// OnCacheFlushItem registers a handler for an entity's flush_item event.
func (p *RedisCacheProvider) OnCacheFlushItem(entityName string, handler func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.flushItemHandlers[entityName]
	if !exists {
		p.flushItemHandlers[entityName] = handler
		go p.listenForFlushItem(entityName)
	}
}

// listenForFlushItem subscribes to an entity's cache_flush_item channel
func (p *RedisCacheProvider) listenForFlushItem(entityName string) {
	time.Sleep(2 * time.Second)
	channel := fmt.Sprintf("cache_flush_item_%s", entityName)
	pubsub := p.client.Subscribe(p.ctx, channel)

	// Track the pubsub so we can close it later
	p.mu.Lock()
	p.pubsubs = append(p.pubsubs, pubsub)
	p.mu.Unlock()

	defer pubsub.Close()

	// Process incoming messages in their own loop
	log.Debugf("Listening for cache_flush_item messages for %s\n", entityName)
	for range pubsub.Channel() {
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

// Close shuts down the provider.
func (p *RedisCacheProvider) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.isClosed {
		return
	}
	log.Infoln("Closing RedisCacheProvider...")
	p.cancel()

	// 👈 Gracefully close all Pub/Sub subscriptions FIRST
	for _, ps := range p.pubsubs {
		_ = ps.Close()
	}

	if p.client != nil {
		_ = p.client.Close()
	}
	p.isClosed = true
}

func (p *RedisCacheProvider) BumpEpoch(entityName string) error {
	if p.isClosed {
		return errors.New("cache provider is closed")
	}
	go func() {
		channel := fmt.Sprintf("cache_bump_epoch_%s", entityName)
		for i := 0; i < 3; i++ {
			err := p.client.Publish(p.ctx, channel, entityName).Err()
			if err == nil {
				p.telemetry.IncCachePubSubPublish(p.options.Addr)
				log.Debugf("Published cache_bump_epoch: %s \n", entityName)
				return
			}
			time.Sleep(5 * time.Second)
		}
	}()
	return nil
}

// 4. Implement OnBumpEpoch and listenForBumpEpoch
func (p *RedisCacheProvider) OnBumpEpoch(entityName string, handler func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.bumpEpochHandlers[entityName]; !exists {
		p.bumpEpochHandlers[entityName] = handler
		go p.listenForBumpEpoch(entityName)
	}
}

func (p *RedisCacheProvider) listenForBumpEpoch(entityName string) {
	time.Sleep(2 * time.Second)
	channel := fmt.Sprintf("cache_bump_epoch_%s", entityName)
	pubsub := p.client.Subscribe(p.ctx, channel)

	p.mu.Lock()
	p.pubsubs = append(p.pubsubs, pubsub)
	p.mu.Unlock()

	defer pubsub.Close()

	for range pubsub.Channel() {
		p.mu.RLock()
		handler, exists := p.bumpEpochHandlers[entityName]
		p.mu.RUnlock()
		if exists {
			handler()
			p.telemetry.IncCachePubSubReceive(p.options.Addr)
		}
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
