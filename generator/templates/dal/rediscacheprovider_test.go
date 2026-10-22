package dal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestRedisCacheProvider spins up an isolated Redis container and verifies both
// the L2 Pub/Sub invalidation channels and the Write-Behind Hash buffer operations.
func TestRedisCacheProvider(t *testing.T) {
	ctx := context.Background()

	// Create a Redis container.
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("failed to start redis container: %v", err)
	}
	// Ensure the container is terminated at the end of the test.
	defer func() {
		if err := redisC.Terminate(ctx); err != nil {
			t.Fatalf("failed to terminate redis container: %v", err)
		}
	}()

	// Get the host and mapped port.
	host, err := redisC.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get container host: %v", err)
	}
	port, err := redisC.MappedPort(ctx, "6379")
	if err != nil {
		t.Fatalf("failed to get container port: %v", err)
	}

	addr := fmt.Sprintf("%s:%s", host, port.Port())
	t.Logf("Address of redis is: %s", addr)

	// Initialize the provider via constructor injection (passing nil uses NoopTelemetryProvider).
	provider := NewRedisCacheProvider(addr, "", 0, nil)
	if err := provider.Connect(); err != nil {
		t.Fatalf("failed to connect to redis: %v", err)
	}
	defer provider.Close()
	t.Logf("Connected to redis: %s", addr)

	t.Run("PubSub Operations", func(t *testing.T) {
		// 1. Single-Key Cache Invalidation
		// ARCHITECTURE: Because OnCacheInvalidated synchronously awaits the Redis SUBSCRIBE
		// handshake before returning, we can immediately publish without race conditions or sleeps.
		resultCh := make(chan string, 1)
		provider.OnCacheInvalidated("test", func(key string) {
			resultCh <- key
		})

		resultCh2 := make(chan string, 1)
		provider.OnCacheInvalidated("test2", func(key string) {
			resultCh2 <- key
		})

		if err := provider.InvalidateCache("test", "key1"); err != nil {
			t.Fatalf("InvalidateCache returned error: %v", err)
		}
		if err := provider.InvalidateCache("test2", "key2"); err != nil {
			t.Fatalf("InvalidateCache returned error: %v", err)
		}

		// Wait for both handlers to be invoked.
		receivedCount := 0
		timeout := time.After(5 * time.Second)
		for receivedCount < 2 {
			select {
			case received := <-resultCh:
				if received != "key1" {
					t.Errorf("expected 'key1', got '%s'", received)
				}
				receivedCount++
			case received := <-resultCh2:
				if received != "key2" {
					t.Errorf("expected 'key2', got '%s'", received)
				}
				receivedCount++
			case <-timeout:
				// Use t.Fatal so the test aborts immediately instead of looping until the 30s runner panic.
				t.Fatal("timeout waiting for cache invalidation handler")
			}
		}

		// 2. FlushListCache
		flushListCh := make(chan struct{}, 1)
		provider.OnCacheFlushList("test", func() {
			flushListCh <- struct{}{}
		})
		if err := provider.FlushListCache("test"); err != nil {
			t.Fatalf("FlushListCache returned error: %v", err)
		}

		select {
		case <-flushListCh:
			// Success
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for cache flush list handler")
		}

		// 3. FlushItemCache (used during bulk updates/upserts/deletes)
		flushItemCh := make(chan struct{}, 1)
		provider.OnCacheFlushItem("test", func() {
			flushItemCh <- struct{}{}
		})
		if err := provider.FlushItemCache("test"); err != nil {
			t.Fatalf("FlushItemCache returned error: %v", err)
		}

		select {
		case <-flushItemCh:
			// Success
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for cache flush item handler")
		}

		// 4. BumpEpoch (used for O(1) list cache invalidation across horizontal pods)
		bumpEpochCh := make(chan struct{}, 1)
		provider.OnBumpEpoch("test", func() {
			bumpEpochCh <- struct{}{}
		})
		if err := provider.BumpEpoch("test"); err != nil {
			t.Fatalf("BumpEpoch returned error: %v", err)
		}

		select {
		case <-bumpEpochCh:
			// Success
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for bump epoch handler")
		}
	})

	t.Run("Buffer Operations", func(t *testing.T) {
		testHashKey := "test:buffer:counts"
		testField := "match_xyz"

		// 1. Test HIncrBy (Creates the hash and atomically increments)
		if err := provider.HIncrBy(ctx, testHashKey, testField, 5); err != nil {
			t.Fatalf("HIncrBy failed: %v", err)
		}

		if err := provider.HIncrBy(ctx, testHashKey, testField, 2); err != nil {
			t.Fatalf("HIncrBy failed: %v", err)
		}

		// 2. Test HScan (Should retrieve "match_xyz" with value "7")
		results, cursor, err := provider.HScan(ctx, testHashKey, 0, "*", 100)
		if err != nil {
			t.Fatalf("HScan failed: %v", err)
		}
		if cursor != 0 {
			t.Errorf("Expected cursor 0, got %d", cursor)
		}
		if len(results) != 2 || results[0] != testField || results[1] != "7" {
			t.Errorf("HScan returned unexpected results: %v", results)
		}

		// 3. Test Rename (Used by Flusher to isolate the active buffer snapshot)
		newHashKey := "test:buffer:processing"
		if err := provider.Rename(ctx, testHashKey, newHashKey); err != nil {
			t.Fatalf("Rename failed: %v", err)
		}

		// Verify old key is gone
		results, _, err = provider.HScan(ctx, testHashKey, 0, "*", 100)
		if err != nil {
			t.Fatalf("HScan failed: %v", err)
		}
		if len(results) != 0 {
			t.Errorf("Expected old hash key to be empty after rename")
		}

		// 4. Test SetNX (Distributed locking mechanism for multi-pod flushers)
		lockKey := "test:flush_lock"
		acquired, err := provider.SetNX(ctx, lockKey, "locked", 10*time.Second)
		if err != nil {
			t.Fatalf("SetNX failed: %v", err)
		}
		if !acquired {
			t.Errorf("Failed to acquire initial SetNX lock")
		}

		// Second attempt should fail to acquire while lock is held
		acquired2, err := provider.SetNX(ctx, lockKey, "locked", 10*time.Second)
		if err != nil {
			t.Fatalf("Second SetNX failed: %v", err)
		}
		if acquired2 {
			t.Errorf("Second SetNX should not have acquired the lock")
		}

		// 5. Test Del (Cleanup)
		if err := provider.Del(ctx, newHashKey, lockKey); err != nil {
			t.Fatalf("Del failed: %v", err)
		}

		// Verify lock is released
		acquired3, err := provider.SetNX(ctx, lockKey, "locked", 10*time.Second)
		if err != nil {
			t.Fatalf("Third SetNX failed: %v", err)
		}
		if !acquired3 {
			t.Errorf("Expected to acquire lock after deletion")
		}
	})

	t.Run("Close Guardrails", func(t *testing.T) {
		provider.Close()

		// Publishing after Close() should immediately return an error without spawning goroutines.
		if err := provider.InvalidateCache("test", "key1"); err == nil {
			t.Errorf("expected error when calling InvalidateCache on closed provider, got nil")
		}
		if err := provider.FlushListCache("test"); err == nil {
			t.Errorf("expected error when calling FlushListCache on closed provider, got nil")
		}
		if err := provider.FlushItemCache("test"); err == nil {
			t.Errorf("expected error when calling FlushItemCache on closed provider, got nil")
		}
		if err := provider.BumpEpoch("test"); err == nil {
			t.Errorf("expected error when calling BumpEpoch on closed provider, got nil")
		}
	})
}
