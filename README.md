```markdown
# DALForge 🛠️

[![Go Reference](https://pkg.go.dev/badge/github.com/purplehoneyapp/dalforge.svg)](https://pkg.go.dev/github.com/purplehoneyapp/dalforge)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-%3E%3D%201.24-00ADD8.svg)](https://golang.org/doc/devel/release.html)

**DALForge** is a CLI tool that generates a highly optimized, production-ready Data Access Layer (DAL) in Go from simple YAML configuration files. 

It eliminates database boilerplate by transforming YAML entity definitions into robust, interface-driven Go code equipped with built-in telemetry, circuit breaking, advanced multi-tier caching, and asynchronous Write-Behind background workers.

## ✨ Features

- **Clean Architecture Enforced:** Strictly decouples domain rules from infrastructure by generating isolated interface contracts (`.gen.go`) and hidden concrete implementations (`_impl.gen.go`).
- **Zero-Boilerplate CRUD:** Automatically generates `Create`, `CreateBulk`, `Update`, `Get`, `List`, and `Delete` operations based on your schema.
- **Advanced Bulk Operations:** Native support for bulk gets (`IN` clauses), bulk partial updates, and bulk lists with mixed scalar and variadic parameters.
- **Write-Behind Caching (NEW):** Native support for `bufferedCounters` offering O(1) Redis atomic increments. Auto-generates distributed background workers (`_flusher.gen.go`) to safely batch-upsert high-velocity metrics (like gamification points) to MySQL, eliminating row-level lock contention.
- **Multi-Tier Caching:** Implements an L1 in-memory cache (`go-cache`) synchronized across instances via an L2 Redis Pub/Sub invalidation layer.
- **Scatter-Gather Cache Pattern:** Bulk `Get` operations automatically check the local cache first and only query the database for cache misses, saving immense DB load.
- **Resilience Built-In:** All database calls are wrapped in [gobreaker](https://github.com/sony/gobreaker) circuit breakers to prevent cascading failures.
- **Observability:** Built-in Prometheus telemetry tracking latency, cache hit/miss ratios, circuit breaker states, and database errors.
- **Soft Deletes & Unique Scrambling:** Native support for `deleted_at` scoping, complete with unique-key scrambling to prevent collisions upon re-registration.

## 🚀 Installation

Ensure you have [Go](https://golang.org/doc/install) installed (version 1.24+ recommended).

```bash
go install [github.com/purplehoneyapp/dalforge@latest](https://github.com/purplehoneyapp/dalforge@latest)

```

## 🛠️ Quick Start

1. Create a YAML definition file (`config/match_reaction_counter.yaml`):

```yaml
name: match_reaction_counter
version: v1
columns:
  match_uid:
    type: varchar
    unique: true
    allowNull: false
  reaction_count:
    type: int32
    allowNull: false
operations:
  write: true
  delete: true
  gets:
    - match_uid
  getsBulk:
    - match_uid
  
  # Write-Behind buffer flush (Relative delta increment)
  upsertsBulk:
    - name: upsert_reaction_counts
      conflictTarget: match_uid
      updateColumns:
        - reaction_count
      increment: true 

# Auto-generates an O(1) Redis incrementer and a background MySQL sync worker
bufferedCounters:
  - name: increment_reaction
    column: reaction_count
    groupBy: match_uid
    flushIntervalSeconds: 300

circuitbreaker:
  timeoutSeconds: 20
  consecutiveFailures: 4
caching:
  type: memory
  singleExpirationSeconds: 300
  listExpirationSeconds: 60
  listInvalidation: expire
  maxItemsCount: 100000

```

2. Run DALForge:

```bash
dalforge generate ./config ./internal/dal

```

3. Use your generated code:
DALForge generates three files per entity to enforce Clean Architecture:

* `match_reaction_counter.gen.go` (The interface contract and pure Go structs)
* `match_reaction_counter_impl.gen.go` (The private MySQL/Redis implementation)
* `match_reaction_counter_flusher.gen.go` (The isolated background sync worker)

You can immediately use the repository in your service layer to absorb high-velocity events without crashing your database:

```go
repo := dal.NewMatchReactionCounterRepository(dbProvider, redisCache, configProvider, cbSettings, telemetry)

// O(1) Redis Increment - Safely buffer thousands of gamification events per second!
err := repo.IncrementReaction(ctx, "match_abc_123", 1)

// Start the isolated background worker to flush to MySQL every 5 minutes
flusher := dal.NewIncrementReactionFlusher(repo, redisCache, telemetry)
flusher.Start(ctx)

```

## 📖 Configuration Guide

### Supported Column Types

DALForge maps YAML types to native Go and SQL types automatically:
`int8`, `int16`, `int32`, `int64`, `float`, `varchar`, `text`, `bool`, `date`, `time`, `datetime`, `uid`, `json`.

### The operations Block

Define exactly what queries your repository needs. Unused operations are not generated, keeping your binary small.

* `write`: Generates `Create`, `CreateBulk`, and `Update`.
* `delete` / `softDelete`: Generates `Delete` (and `HardDelete`). Soft deletes automatically scope all gets and lists with `deleted_at IS NULL`.
* `gets`: Generates single-item fetchers (e.g., `GetByEmail`). Fields must be marked `unique: true`.
* `getsBulk`: Generates scatter-gather `IN` clause fetchers (e.g., `GetByUids`).
* `lists`: Generates paginated `SELECT` queries with custom `where` clauses.
* `listsBulk`: Generates `IN` clause lists. Supports mixing standard `where` scalars with a variadic `whereIn` parameter.
* `updatesBulk`: Generates highly optimized bulk partial updates (e.g., `UPDATE users SET status = ? WHERE uid IN (...)`).
* `upsertsBulk`: Generates `INSERT ... ON DUPLICATE KEY UPDATE` queries. Can be configured for absolute state overwrites (`increment: false`) or relative delta math (`increment: true`).
* `deletes`: Generates custom bulk delete operations (e.g., `DeleteExpired`).
* `plucks`: Generates queries that return a slice of a single column's values, rather than full entity structs. This is highly optimized for retrieving lists of identifiers. You specify a `name`, the target `column`, and a custom `where` clause (which supports named parameters mapped via `typeMapping`).
* *Example Usage:* Extracting all active `story_uid`s associated with a specific user.



### Write-Behind Caching (The `bufferedCounters` block)

For gamified systems where users generate rapid events (such as sending emoticons or accumulating Weekly League points), updating MySQL synchronously causes severe row-level lock contention.

Declaring a `bufferedCounters` block instructs DALForge to generate an `Increment` method that utilizes a non-blocking Redis `HINCRBY`. It also scaffolds a dedicated background worker (`_flusher.gen.go`) that utilizes distributed locks (`SetNX`) and cursor iteration (`HSCAN`) to aggregate and flush the buffer to MySQL via bulk upserts.

### Telemetry & Circuit Breaking

DALForge strictly enforces safety. Bulk operations are hard-limited to 5000 items and automatically chunked into database queries of 500 parameters to prevent driver panics.

All generated operations report directly to a `TelemetryProvider` interface, allowing you to easily mock metrics in testing or bind them to Prometheus in production.

## 🧪 Testing

DALForge guarantees the validity of generated code. The generated templates themselves are heavily tested within this repository against real MySQL and Redis testcontainers.

Because the generator is tested, you do not need to write unit tests for the generated DAL code in your own projects. Simply mock the generated Interfaces in your service-layer tests.

To run the internal test suite:

```bash
go test ./...

```

## 🤝 Contributing

We welcome contributions! Whether it's a bug report, a new feature, or documentation improvements:

1. Fork the repository.
2. Create a new branch (`git checkout -b feature/amazing-feature`).
3. Commit your changes (`git commit -m 'Add amazing feature'`).
4. Push to the branch (`git push origin feature/amazing-feature`).
5. Open a Pull Request.

Please ensure your code passes existing tests and includes new tests for added features.

## 📄 License

DALForge is open-source software released under the MIT License.

```

```