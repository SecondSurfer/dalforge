# DALForge 🛠️

[![Go Reference](https://pkg.go.dev/badge/github.com/purplehoneyapp/dalforge.svg)](https://pkg.go.dev/github.com/purplehoneyapp/dalforge)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-%3E%3D%201.24-00ADD8.svg)](https://golang.org/doc/devel/release.html)

**DALForge** is a CLI tool that generates a highly optimized, production-ready Data Access Layer (DAL) in Go from simple YAML configuration files. 

It eliminates database boilerplate by transforming YAML entity definitions into robust, interface-driven Go code equipped with built-in telemetry, circuit breaking, advanced multi-tier caching, asynchronous Write-Behind background workers, and a declarative schema migrator.

## ✨ Features

- **Clean Architecture Enforced:** Strictly decouples domain rules from infrastructure by generating isolated interface contracts (`.gen.go`) and hidden concrete implementations (`_impl.gen.go`).
- **Declarative Schema Migrator (NEW):** Auto-generates an embedded SQL filesystem (`schema_embed.gen.go`) and a safe, interactive migration engine (`migrator.gen.go`) powered by `mysqldef`—complete with dry-run diffs, destructive `DROP` warnings, and environment-specific confirmation gates.
- **Zero-Boilerplate CRUD:** Automatically generates `Create`, `CreateBulk`, `Update`, `Get`, `List`, and `Delete` operations based on your schema. All `unique: true` columns automatically receive dedicated `GetBy<Column>` point-read fetchers and secondary cache invalidation.
- **Smart SQL Index Optimization:** Automatically generates covering B-Tree indexes for your queries while pruning redundant left-prefix indexes (e.g., dropping `(age)` if `(age, created)` exists) and validating against invalid `TEXT`/`JSON` key specifications.
- **Advanced Bulk Operations:** Native support for bulk gets (`IN` clauses), bulk partial updates, and bulk lists with mixed scalar and variadic parameters.
- **Write-Behind Caching:** Native support for `bufferedCounters` offering $\mathcal{O}(1)$ Redis atomic increments. Auto-generates distributed background workers (`_flusher.gen.go`) to safely batch-upsert high-velocity metrics (like gamification points) to MySQL, eliminating row-level lock contention.
- **Multi-Tier Caching:** Implements an L1 in-memory cache (`go-cache`) synchronized across instances via an L2 Redis Pub/Sub invalidation layer.
- **Scatter-Gather Cache Pattern:** Bulk `Get` and paginated `List` operations automatically check the local cache first and only query the database for cache misses, saving immense DB load.
- **Resilience Built-In:** All database calls are wrapped in [gobreaker](https://github.com/sony/gobreaker) circuit breakers to prevent cascading failures.
- **Observability:** Built-in Prometheus telemetry tracking latency, cache hit/miss ratios, circuit breaker states, and database errors.
- **Soft Deletes & Unique Scrambling:** Native support for `deleted_at` scoping, complete with unique-key scrambling to prevent collisions upon re-registration.

## 🚀 Installation

Ensure you have [Go](https://golang.org/doc/install) installed (version 1.24+ recommended).

```bash
go install github.com/purplehoneyapp/dalforge@latest
```

*(Optional)* If you plan to use the generated **Schema Migrator**, install `mysqldef` so it is available in your `PATH`:

```bash
go install github.com/sqldef/sqldef/cmd/mysqldef@latest
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
DALForge generates entity-specific files along with shared infrastructure providers to enforce Clean Architecture:

* `match_reaction_counter.gen.go` (The interface contract and pure Go structs)
* `match_reaction_counter_impl.gen.go` (The private MySQL/Redis implementation)
* `match_reaction_counter_flusher.gen.go` (The isolated background sync worker)
* `match_reaction_counter.sql` (The complete DDL schema and optimized indexes)

You can immediately use the repository in your service layer to absorb high-velocity events without crashing your database:

```go
repo := dal.NewMatchReactionCounterRepository(dbProvider, redisCache, configProvider, cbSettings, telemetry)

// O(1) Redis Increment - Safely buffer thousands of gamification events per second!
err := repo.IncrementReaction(ctx, "match_abc_123", 1)

// Start the isolated background worker to flush to MySQL every 5 minutes
flusher := dal.NewIncrementReactionFlusher(repo, redisCache, telemetry)
flusher.Start(ctx)
```

---

## 🗄️ Declarative Schema Migrator

When you run `dalforge generate`, DALForge outputs a `.sql` file for each entity alongside two shared migration files in your target package:
* `schema_embed.gen.go`: Uses `//go:embed *.sql` to compile all generated `.sql` table and index definitions directly into your Go binary (`dal.NewSchemaFS()`).
* `migrator.gen.go`: Provides `dal.Migrator` and `dal.MysqldefCLIExecutor` to diff your embedded SQL schema against a live MySQL database and apply changes declaratively.

### Why Embed SQL in the Binary?
By embedding the generated `.sql` files at compile time, a binary or Docker image tagged `v1.2.3` permanently contains the exact database schema of `v1.2.3`. You never have to maintain sequential `001_up.sql` / `002_down.sql` migration files by hand or worry about schema drift between your code and external SQL files on disk.

### What the Migrator Does
When `migrator.Run(ctx)` is executed:
1. **Loads & Sanitizes Embedded SQL:** Reads all `*.sql` files from `dal.NewSchemaFS()`, strips internal `#` template comments, and concatenates them into a single desired DDL state.
2. **Executes a Dry-Run Diff:** Invokes `mysqldef --dry-run` against the target MySQL database (passing `MYSQL_PWD` via environment variable so credentials never leak in process tables like `ps aux`).
3. **Filters Shared-Database Noise:** Automatically separates executable DDL statements from `-- Skipped:` comments (useful when multiple microservices share a database and `--skip-drop` is enabled). If no changes are needed, it exits cleanly with a `100% in sync` message without prompting.
4. **Highlights Destructive Operations:** Prints the exact `ALTER` / `CREATE` / `DROP` plan to the terminal and emits a high-visibility warning if any destructive `DROP` statements are detected.
5. **Enforces Interactive Approval Gates:**
   - For standard environments (`dev`, `staging`), the operator must type `yes` to proceed.
   - For production (`EnvName: "prod"` or `"PROD"`), the migrator enforces a stricter guard: the operator must explicitly type `apply-prod` (typing `yes` will safely abort).
6. **Applies the Schema:** Respects `context.Context` cancellation (`SIGINT`/`SIGTERM`) and applies the idempotent DDL changes to the target database.

### How to Use It in Your Code

Create a dedicated CLI entrypoint (e.g., `cmd/migrate/main.go`) in your service and wire the generated `dal.Migrator` using constructor injection:

```go
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"yourproject/internal/dal"
)

// Version can be injected at build time via -ldflags "-X main.Version=v1.2.3"
var Version = "dev"

func main() {
	env := flag.String("env", "dev", "Target environment (dev, staging, prod)")
	host := flag.String("host", "127.0.0.1", "MySQL host (or local SSH tunnel host)")
	port := flag.Int("port", 3306, "MySQL port")
	user := flag.String("user", "root", "MySQL user")
	pass := flag.String("pass", "", "MySQL password")
	dbName := flag.String("db", "myapp", "MySQL database name")
	skipDrop := flag.Bool("skip-drop", false, "Skip destructive DROP statements")
	flag.Parse()

	// 1. Listen for Ctrl+C / SIGTERM for safe cancellation before apply
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 2. Populate the pure configuration struct
	cfg := dal.MigrationConfig{
		AppName:  "MyApp Schema Migrator",
		EnvName:  *env,
		Version:  Version,
		Host:     *host,
		Port:     *port,
		User:     *user,
		Password: *pass,
		Database: *dbName,
		SkipDrop: *skipDrop,
	}

	// 3. Wire the embedded schema filesystem, mysqldef executor, and stdio streams
	schemaFS := dal.NewSchemaFS()
	executor := dal.NewMysqldefCLIExecutor(cfg)
	migrator := dal.NewMigrator(schemaFS, executor, cfg, os.Stdin, os.Stdout)

	// 4. Run the interactive dry-run and migration flow
	if err := migrator.Run(ctx); err != nil {
		log.Fatalf("Migration failed: %v", err)
	}
}
```

### Example Terminal Output

Running your migration command produces an interactive preview before any DDL is executed:

```text
================================================================================
🐝 MyApp Schema Migrator | Release Version: v1.5.7
🌍 Environment : PROD
📦 Embedded SQL: 39 table definitions locked in binary
🗄️  Target MySQL: root@127.0.0.1:3306/myapp
================================================================================

📋 Proposed DDL Changes for [PROD] (Release v1.5.7):
--------------------------------------------------------------------------------
ALTER TABLE `users` ADD COLUMN `order` TINYINT NOT NULL AFTER `age`;
CREATE INDEX `idx_age_created` ON `users` (`age`, `created`);
--------------------------------------------------------------------------------

🚨 PRODUCTION WARNING: Type 'apply-prod' to execute these changes on PROD: apply-prod

⚡ Applying schema migrations to [PROD]...
ALTER TABLE `users` ADD COLUMN `order` TINYINT NOT NULL AFTER `age`;
CREATE INDEX `idx_age_created` ON `users` (`age`, `created`);
✅ Migration for release [v1.5.7] successfully applied to [PROD]!
```

---

## 📖 Configuration Guide

### Supported Column Types

DALForge maps YAML types to native Go and SQL types automatically:
`int8`, `int16`, `int32`, `int64`, `float`, `varchar`, `text`, `bool`, `date`, `time`, `datetime`, `uid`, `json`.

> **Note on Unique & Indexed Columns:**
> - Any column marked `unique: true` must be a non-nullable `varchar` or `uid` type and is automatically added to `operations.gets` (`GetBy<Column>`).
> - Columns of type `text` or `json` cannot be used in `lists`, `listsBulk`, `updatesBulk`, `plucks`, or `deletes` where clauses/orders, as MySQL cannot build B-Tree indexes on `TEXT`/`JSON` columns without a key length (MySQL Error 1170).

### The `operations` Block

Define exactly what queries your repository needs. Unused operations are not generated, keeping your binary small.

* `write`: Generates `Create`, `CreateBulk`, and `Update`.
* `delete` / `softDelete`: Generates `Delete` (and `HardDelete`). Soft deletes automatically scope all gets and lists with `deleted_at IS NULL` and scramble unique string columns (`CONCAT(col, '-del-', UUID())`) so returning users can re-register without unique constraint collisions.
* `gets`: Generates single-item fetchers (e.g., `GetByEmail`). Any column marked `unique: true` is automatically included here.
* `getsBulk`: Generates scatter-gather `IN` clause fetchers (e.g., `GetByUids`).
* `lists`: Generates paginated `SELECT` and `Count` queries with custom `where` clauses. Automatically injects `id` into `getsBulk` to support scatter-gather cache resolution.
* `listsBulk`: Generates `IN` clause lists. Supports mixing standard `where` scalars with a variadic `whereIn` parameter.
* `updatesBulk`: Generates highly optimized bulk partial updates (e.g., `UPDATE users SET status = ? WHERE uid IN (...)`).
* `upsertsBulk`: Generates `INSERT ... ON DUPLICATE KEY UPDATE` queries. Can be configured for absolute state overwrites (`increment: false`) or relative delta math (`increment: true`).
* `deletes`: Generates custom bulk delete operations (e.g., `DeleteExpired`) clamped to a safe batch limit of 5000 rows per call.
* `plucks`: Generates queries that return a slice of a single column's values rather than full entity structs. This is highly optimized for retrieving lists of identifiers using covering indexes. You specify a `name`, the target `column`, and a custom `where` clause (which supports named parameters mapped via `typeMapping`).
  * *Example Usage:* Extracting all active `story_uid`s associated with a specific user.

### Write-Behind Caching (The `bufferedCounters` block)

For gamified systems where users generate rapid events (such as sending emoticons or accumulating Weekly League points), updating MySQL synchronously causes severe row-level lock contention.

Declaring a `bufferedCounters` block instructs DALForge to generate an `Increment` method that utilizes a non-blocking Redis `HINCRBY`. It also scaffolds a dedicated background worker (`_flusher.gen.go`) that utilizes distributed locks (`SetNX`), atomic snapshot isolation (`RENAME`), and cursor iteration (`HSCAN`) to aggregate and flush the buffer to MySQL via bulk upserts.

### Telemetry & Circuit Breaking

DALForge strictly enforces safety. Bulk operations are hard-limited to 5000 items and automatically chunked into database queries of 500 parameters to prevent driver panics.

All generated operations report directly to a `TelemetryProvider` interface, allowing you to easily mock metrics in testing (`NoopTelemetryProvider`) or bind them to Prometheus in production (`PrometheusTelemetryProvider`).

## 🧪 Testing

DALForge guarantees the validity of generated code. The generated templates—including repositories, Redis Pub/Sub invalidation, write-behind flushers, and the schema migrator—are heavily tested within this repository against real MySQL and Redis testcontainers.

Because the generator is tested, you do not need to write unit tests for the generated DAL code in your own projects. Simply mock the generated Repository interfaces in your service-layer tests.

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