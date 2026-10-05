Here is the comprehensive technical implementation document for the `dalforge` engineering team.

---

# Technical Specification: Dalforge Write-Behind Cache & Architectural Split

## 1. Executive Summary

As PurpleHoney scales, our backend must handle high-velocity asynchronous events—such as users sending predefined emoticons (no custom chats allowed) and accumulating gamification points for the League. Updating MySQL directly for every reaction or point triggers row-level locks and N+1 cache invalidations.

To solve this, we are upgrading the `dalforge` generator to natively scaffold a **Write-Behind Cache (Pattern 1)**. This allows the Service layer to execute non-blocking, O(1) increments in Redis, while a generated background worker safely flushes aggregates to MySQL using `HSCAN` and bulk upserts. Additionally, we are splitting the generated output into separate interface and implementation files to strictly enforce Clean Architecture and optimize token windows for AI agents.

---

## 2. Requirement 1: Architectural Output Split

**Goal:** Decouple domain contracts from infrastructure implementation.

Currently, `dalforge` outputs a single `{entity}.gen.go` file containing both the interface and the Redis/MySQL logic.
**Action Required:** Modify `dalforge/generator/gofiles.go` to parse the YAML (e.g., `user.yaml`) and execute two separate templates per entity:

1. **`{entity}.gen.go` (The Contract):**
* Contains *only* the domain `struct` and the `Repository` interface.
* **Why:** The Use Case (Service) layer should only ever import this file. This provides a clean, 50-line file for AI agents to process business rules without parsing thousands of lines of SQL strings.


2. **`{entity}_impl.gen.go` (The Implementation):**
* Contains the private struct (e.g., `type userRepository struct`), MySQL execution, Redis caching, telemetry, and Circuit Breaker logic.



---

## 3. Requirement 2: Support for `upsertsBulk`

**Goal:** Eliminate N+1 query bottlenecks during background synchronization.

To efficiently flush hundreds of buffered increments to the database in a single network round-trip, `dalforge` must support `INSERT ... ON DUPLICATE KEY UPDATE` operations.

**Action Required:** Add `upsertsBulk` to the schema parser.

**YAML Specification:**

```yaml
upsertsBulk:
  - name: upsert_reaction_counts
    conflictTarget: match_uid
    updateColumns:
      - reaction_count
    increment: true # If true, generates: column = column + VALUES(column)

```

**Generated Output:** The template must generate a bulk SQL statement mapping the slice of entities into a parameterized `INSERT` with the appropriate `ON DUPLICATE KEY UPDATE reaction_count = reaction_count + VALUES(reaction_count)` clause.

---

## 4. Requirement 3: The `bufferedCounters` Extension

**Goal:** Abstract the Write-Behind Cache pattern into a declarative YAML block.

**Action Required:** Extend the `dalforge` schema to recognize a `bufferedCounters` configuration block on an entity.

**YAML Specification:**

```yaml
bufferedCounters:
  - name: increment_reaction
    column: reaction_count
    groupBy: match_uid
    flushIntervalSeconds: 300

```

Note: Entities using `bufferedCounters` must set `listInvalidation: none` in their `caching` block. If set to `flush` or `epoch`, the background worker will wipe global lists every 300 seconds.

---

## 5. Requirement 4: Generated Buffer Increment (Redis)

**Goal:** Provide an O(1) atomic increment method to the Service layer.

When the `bufferedCounters` block is detected, the generator must scaffold an interface method and a Redis-backed implementation.

**Action Required:** Generate the following method in `{entity}_impl.gen.go`. It must **not** execute SQL; it must execute a Redis `HINCRBY`.

**Generated Output:**

```go
// IncrementReaction executes an O(1) in-memory increment to be flushed later.
func (d *matchReactionCounterRepository) IncrementReaction(ctx context.Context, matchUid string, delta int) error {
    bufferKey := "purplehoney:match_reaction_counter:reaction_count:buffer"
    
    // Non-blocking hash increment
    if err := d.redisClient.HIncrBy(ctx, bufferKey, matchUid, int64(delta)).Err(); err != nil {
        d.telemetryProvider.IncDBError("match_reaction_counter", "increment_buffer")
        return err
    }
    return nil
}

```

---

## 6. Requirement 5: Generated Flusher Worker

**Goal:** Automatically scaffold the infrastructure worker that drains the Redis buffer to MySQL.

**Action Required:** If `bufferedCounters` is present, `dalforge` must generate a standalone file (e.g., `match_reaction_counter_flusher.gen.go`). This worker must:

1. Implement a `time.Ticker` loop based on `flushIntervalSeconds`.
2. Use a distributed lock to ensure only one API instance processes the flush.
3. Use `RENAME` on the Redis buffer key to safely isolate the current snapshot without dropping incoming increments.
4. Use **`HSCAN` (NOT `HGETALL`)** to iterate over the keys. Redis is single-threaded; `HGETALL` on a large dataset during a traffic spike will block the event loop and crash the application.
5. Pass the chunked results into the `upsertsBulk` method defined in Requirement 2.

**Generated Output (Conceptual Logic):**

```go
func (f *MatchReactionCounterFlusher) flush(ctx context.Context) {
    // 1. Acquire Lock...
    // 2. Rename bufferKey to processingKey...

    var cursor uint64
    var batch []*MatchReactionCounter

    // 3. HSCAN iteration
    for {
        var results []string
        var err error
        
        results, cursor, err = f.redisClient.HScan(ctx, processingKey, cursor, "*", 1000).Result()
        if err != nil { return }

        for i := 0; i < len(results); i += 2 {
            identifier := results[i]
            countStr := results[i+1]
            count, _ := strconv.ParseInt(countStr, 10, 32)
            
            batch = append(batch, &MatchReactionCounter{
                MatchUid: identifier, 
                ReactionCount: int32(count),
            })
        }

        if cursor == 0 { break }
    }

    // 4. Execute O(1) Bulk SQL Upsert
    if len(batch) > 0 {
        _ = f.repo.UpsertReactionCounts(ctx, batch)
    }

    // 5. Delete processingKey...
}

```

By completing this specification, the `dalforge` tool will automatically handle the massive write velocity required for timeline reactions (capped at a 100-item rolling window) and League gamification, keeping the Use Case (Service) layer 100% decoupled from infrastructure mechanics.