package dal

import (
	"context"
	"time"
)

// CacheProvider interface defines the required methods.
type CacheProvider interface {
	Connect() error
	InvalidateCache(entityName, cacheKey string) error
	FlushListCache(entityName string) error
	FlushItemCache(entityName string) error
	OnCacheInvalidated(entityName string, handler func(string))
	OnCacheFlushList(entityName string, handler func())
	OnCacheFlushItem(entityName string, handler func())
	BumpEpoch(entityName string) error
	OnBumpEpoch(entityName string, handler func())
	Close()

	// Buffer Operations for Write-Behind Cache
	HIncrBy(ctx context.Context, key, field string, incr int64) error
	HScan(ctx context.Context, key string, cursor uint64, match string, count int64) ([]string, uint64, error)
	Rename(ctx context.Context, oldKey, newKey string) error
	Del(ctx context.Context, keys ...string) error
	SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error)
}

// Default Cache Provider that does not use Redis. If DAL entity is not provided with cache provider this one
// will be used.
type NoopCacheProvider struct{}

func (d NoopCacheProvider) Connect() error {
	return nil
}

func (d NoopCacheProvider) Close() {}

func (d NoopCacheProvider) InvalidateCache(entityName, cacheKey string) error {
	return nil
}

func (d NoopCacheProvider) FlushListCache(entityName string) error {
	return nil
}

func (d NoopCacheProvider) FlushItemCache(entityName string) error { // <-- NEW
	return nil
}

func (d NoopCacheProvider) OnCacheInvalidated(entityName string, handler func(string)) {}
func (d NoopCacheProvider) OnCacheFlushList(entityName string, handler func())         {}
func (d NoopCacheProvider) OnCacheFlushItem(entityName string, handler func())         {} // <--
func (d NoopCacheProvider) BumpEpoch(entityName string) error                          { return nil }
func (d NoopCacheProvider) OnBumpEpoch(entityName string, handler func())              {}

// Dummy Buffer implementations
func (d NoopCacheProvider) HIncrBy(ctx context.Context, key, field string, incr int64) error {
	return nil
}
func (d NoopCacheProvider) HScan(ctx context.Context, key string, cursor uint64, match string, count int64) ([]string, uint64, error) {
	return nil, 0, nil
}
func (d NoopCacheProvider) Rename(ctx context.Context, oldKey, newKey string) error { return nil }
func (d NoopCacheProvider) Del(ctx context.Context, keys ...string) error           { return nil }
func (d NoopCacheProvider) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error) {
	return true, nil
}
