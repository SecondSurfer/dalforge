package dal

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sony/gobreaker"
	"github.com/stretchr/testify/assert"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

func TestMatchReactionCounterUpsertBulk(t *testing.T) {
	t.Run("TestUpsertReactionCountsIncrement", func(t *testing.T) {
		setupTestDB(t) // Uses the existing testcontainers setup from user_test.go
		defer teardownTestDB(t)

		// Instantiate the repository
		reactionDAL := NewMatchReactionCounterRepository(
			dbProvider,
			nil, // default cache provider
			nil, // default config provider
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)
		ctx := context.Background()

		matchUid1 := "match_abc_123"
		matchUid2 := "match_xyz_789"

		// ---------------------------------------------------------
		// 1. Initial Insert (Simulating the first flush of a new match)
		// ---------------------------------------------------------
		initialCounters := []*MatchReactionCounter{
			{
				MatchUid:      matchUid1,
				ReactionCount: 5, // User sent 5 winks before the first flush
			},
			{
				MatchUid:      matchUid2,
				ReactionCount: 2,
			},
		}

		err := reactionDAL.UpsertReactionCounts(ctx, initialCounters)
		assert.NoError(t, err)

		// Verify initial creation
		record1, err := reactionDAL.GetByMatchUid(ctx, matchUid1)
		assert.NoError(t, err)
		assert.Equal(t, int32(5), record1.ReactionCount, "Initial insert should set the absolute value")

		record2, err := reactionDAL.GetByMatchUid(ctx, matchUid2)
		assert.NoError(t, err)
		assert.Equal(t, int32(2), record2.ReactionCount)

		// ---------------------------------------------------------
		// 2. Relative Delta Increment (Simulating subsequent flushes)
		// ---------------------------------------------------------
		// In the Service layer, we only track the *new* winks that occurred since the last flush.
		deltaCounters := []*MatchReactionCounter{
			{
				MatchUid:      matchUid1,
				ReactionCount: 3, // +3 new winks
			},
			{
				MatchUid:      matchUid2,
				ReactionCount: 1, // +1 new wink
			},
		}

		err = reactionDAL.UpsertReactionCounts(ctx, deltaCounters)
		assert.NoError(t, err)

		// Verify cache was safely flushed by the bulk operation
		missesBefore := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("match_reaction_counter", "get_by_id"))

		updated1, err := reactionDAL.GetByID(ctx, record1.ID)
		assert.NoError(t, err)

		missesAfter := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("match_reaction_counter", "get_by_id"))
		assert.Greater(t, missesAfter, missesBefore, "Cache should have been flushed, resulting in a database read")

		// ---------------------------------------------------------
		// 3. Verification of Math
		// ---------------------------------------------------------
		// Record 1 should be: 5 (initial) + 3 (delta) = 8
		assert.Equal(t, int32(8), updated1.ReactionCount, "ON DUPLICATE KEY UPDATE should add the delta to the existing value")
		assert.Greater(t, updated1.Version, record1.Version, "Version should increment on upsert")

		// Record 2 should be: 2 (initial) + 1 (delta) = 3
		updated2, _ := reactionDAL.GetByID(ctx, record2.ID)
		assert.Equal(t, int32(3), updated2.ReactionCount)

		// Verify immutable timestamps were respected
		assert.Equal(t, record1.Created.Unix(), updated1.Created.Unix(), "Created timestamp should remain immutable")
		assert.GreaterOrEqual(t, updated1.Updated.Unix(), record1.Updated.Unix(), "Updated timestamp should advance")
	})
}

func TestMatchReactionCounter_BufferAndFlush(t *testing.T) {
	ctx := context.Background()

	// 1. Spin up an isolated Redis container for testing the buffer
	req := testcontainers.ContainerRequest{
		Image:        "redis:7-alpine",
		ExposedPorts: []string{"6379/tcp"},
		WaitingFor:   wait.ForListeningPort("6379/tcp"),
	}
	redisC, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	assert.NoError(t, err)
	defer redisC.Terminate(ctx)

	host, _ := redisC.Host(ctx)
	port, _ := redisC.MappedPort(ctx, "6379")
	redisAddr := fmt.Sprintf("%s:%s", host, port.Port())

	redisProvider := NewRedisCacheProvider(redisAddr, "", 0, PrometheusTelemetryProvider{})
	err = redisProvider.Connect()
	assert.NoError(t, err)
	defer redisProvider.Close()

	// 2. Setup MySQL
	setupTestDB(t)
	defer teardownTestDB(t)

	// Instantiate the repository with the real Redis provider
	reactionDAL := NewMatchReactionCounterRepository(
		dbProvider,
		redisProvider,
		nil,
		gobreaker.Settings{},
		PrometheusTelemetryProvider{},
	)

	// Instantiate the background worker
	flusher := NewIncrementReactionFlusher(reactionDAL, redisProvider, PrometheusTelemetryProvider{})

	t.Run("Success_BufferAndFlush", func(t *testing.T) {
		// Service Layer: User sends multiple winks rapidly over time
		err := reactionDAL.IncrementReaction(ctx, "match_gamma", 1)
		assert.NoError(t, err)
		err = reactionDAL.IncrementReaction(ctx, "match_gamma", 1)
		assert.NoError(t, err)
		err = reactionDAL.IncrementReaction(ctx, "match_delta", 5) // Sent 5 winks
		assert.NoError(t, err)

		// Background Worker: Timer fires, triggering the flush
		flusher.flush(ctx)

		// Verify MySQL has successfully absorbed the aggregated values
		recordGamma, err := reactionDAL.GetByMatchUid(ctx, "match_gamma")
		assert.NoError(t, err)
		assert.Equal(t, int32(2), recordGamma.ReactionCount)

		recordDelta, err := reactionDAL.GetByMatchUid(ctx, "match_delta")
		assert.NoError(t, err)
		assert.Equal(t, int32(5), recordDelta.ReactionCount)
	})

	t.Run("EdgeCase_ConcurrentFlushLock", func(t *testing.T) {
		// Add a new wink to the buffer
		err := reactionDAL.IncrementReaction(ctx, "match_gamma", 1)
		assert.NoError(t, err)

		// Edge Case: Simulate another horizontal pod currently executing the flush (Lock is held)
		lockKey := "purplehoney:match_reaction_counter:reaction_count:flush_lock"
		acquired, err := redisProvider.SetNX(ctx, lockKey, "locked", 2*time.Minute)
		assert.NoError(t, err)
		assert.True(t, acquired)

		// Trigger the background flush - should abort silently because lock is held
		flusher.flush(ctx)

		// Verify MySQL was NOT updated (should still be 2 from the previous subtest)
		recordGamma, err := reactionDAL.GetByMatchUid(ctx, "match_gamma")
		assert.NoError(t, err)
		assert.Equal(t, int32(2), recordGamma.ReactionCount)

		// Release the lock manually to simulate the other pod finishing
		redisProvider.Del(ctx, lockKey)

		// Flush again - should now succeed
		flusher.flush(ctx)
		recordGamma, err = reactionDAL.GetByMatchUid(ctx, "match_gamma")
		assert.NoError(t, err)
		assert.Equal(t, int32(3), recordGamma.ReactionCount) // 2 (initial) + 1 (new delta)
	})

	t.Run("EdgeCase_EmptyBuffer", func(t *testing.T) {
		// Trigger flush with absolutely nothing in the Redis buffer.
		// It should exit gracefully without panicking or passing empty arrays to MySQL.
		flusher.flush(ctx)

		// Verify state remains untouched
		recordGamma, err := reactionDAL.GetByMatchUid(ctx, "match_gamma")
		assert.NoError(t, err)
		assert.Equal(t, int32(3), recordGamma.ReactionCount)
	})

	t.Run("EdgeCase_DatabaseDisconnectDuringDrain", func(t *testing.T) {
		// Pre-load the buffer with new actions
		err := reactionDAL.IncrementReaction(ctx, "match_gamma", 4)
		assert.NoError(t, err)

		// 1. Manually isolate the buffer into the processing key (simulating step 1-3 of flush)
		bufferKey := "purplehoney:match_reaction_counter:reaction_count:buffer"
		processingKey := bufferKey + ":processing"
		err = redisProvider.Rename(ctx, bufferKey, processingKey)
		assert.NoError(t, err)

		// 2. Disconnect MySQL entirely to trigger a failure during UpsertsBulk
		dbProvider.Disconnect()

		// 3. Attempt to drain. It must return true (indicating failure) and NOT delete the processing key.
		failed := flusher.drain(ctx, processingKey)
		assert.True(t, failed)

		// Verify processing key is still safe and sound in Redis
		results, _, _ := redisProvider.HScan(ctx, processingKey, 0, "*", 100)
		assert.Greater(t, len(results), 0, "Processing key should have been retained to prevent data loss")

		// 4. Restore MySQL connection
		err = dbProvider.Connect()
		assert.NoError(t, err)

		// 5. Trigger standard flush. It should recover the leftover processing key and apply it.
		flusher.flush(ctx)

		recordGamma, err := reactionDAL.GetByMatchUid(ctx, "match_gamma")
		assert.NoError(t, err)
		assert.Equal(t, int32(7), recordGamma.ReactionCount) // 3 (previous) + 4 (recovered delta)

		// Verify processing key is finally gone
		results, _, _ = redisProvider.HScan(ctx, processingKey, 0, "*", 100)
		assert.Equal(t, 0, len(results), "Processing key should be deleted after successful recovery")
	})
}
