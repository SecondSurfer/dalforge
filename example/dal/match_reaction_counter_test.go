package dal

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sony/gobreaker"
	"github.com/stretchr/testify/assert"
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
