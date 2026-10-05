# DALForge 🛠️

[![Go Reference](https://pkg.go.dev/badge/github.com/purplehoneyapp/dalforge.svg)](https://pkg.go.dev/github.com/purplehoneyapp/dalforge)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-%3E%3D%201.24-00ADD8.svg)](https://golang.org/doc/devel/release.html)

**DALForge** is a CLI tool that generates a highly optimized, production-ready Data Access Layer (DAL) in Go from simple YAML configuration files. 

It eliminates database boilerplate by transforming YAML entity definitions into robust, interface-driven Go code equipped with built-in telemetry, circuit breaking, and advanced multi-tier caching.

## ✨ Features

- **Zero-Boilerplate CRUD:** Automatically generates `Create`, `CreateBulk`, `Update`, `Get`, `List`, and `Delete` operations based on your schema.
- **Advanced Bulk Operations:** Native support for bulk gets (`IN` clauses), bulk partial updates, and bulk lists with mixed scalar and variadic parameters.
- **Multi-Tier Caching:** Implements an L1 in-memory cache (`go-cache`) synchronized across instances via an L2 Redis Pub/Sub invalidation layer.
- **Scatter-Gather Cache Pattern:** Bulk `Get` operations automatically check the local cache first and only query the database for cache misses, saving immense DB load.
- **Resilience Built-In:** All database calls are wrapped in [gobreaker](https://github.com/sony/gobreaker) circuit breakers to prevent cascading failures.
- **Observability:** Built-in Prometheus telemetry tracking latency, cache hit/miss ratios, circuit breaker states, and database errors.
- **Soft Deletes & Unique Scrambling:** Native support for `deleted_at` scoping, complete with unique-key scrambling to prevent collisions upon re-registration.

## 🚀 Installation

Ensure you have [Go](https://golang.org/doc/install) installed (version 1.24+ recommended).

```
go install [github.com/purplehoneyapp/dalforge@latest](https://github.com/purplehoneyapp/dalforge@latest)
```

## 🛠️ Quick Start
1. Create a YAML definition file (config/user.yaml):

YAML
```
name: user  # entity name, should be singular, snake cased.
version: v1
columns:
  status:
    type: varchar # supported types are: int8, int32, int64, bool, varchar, text, float, date, time, datetime, uid, json
    allowNull: true
  uid:
    type: uid
    prefix: user
    unique: true
  email:
    type: varchar
    allowNull: false
    unique: true
  birthdate:
    type: date
    allowNull: true
  age:
    type: int8
  meta:
    type: json
    allowNull: true
operations:
  write: true   # if true will generate proper write functions
  delete: true  # if true it will generate delete functions
  softDelete: true # if true it will generate deleted_at column and use that one for all soft delete queries. to really delete record HardDelete should be called.
  gets:
    - email   # put a column name here; it will generate GetByEmail function. The intention is that those columns are unique values.
    - uid

  getsBulk:   # Generate GetByUids and GetByIds with in-memory cache checks
    - uid
    - id
    
  updatesBulk: # Generate surgical bulk partial updates using IN (...)
    - name: update_status_by_uids
      set: 
        - status
      whereIn: uid
    - name: update_age_by_ids
      set:
        - age
      whereIn: id

  upsertsBulk:
    - name: upsert_user_snapshots
      conflictTarget: uid
      updateColumns:
        - status
        - meta
      increment: false

  lists:
    - name: list_by_id                 # name of a function that will fetch multiple rows with pagination supported. This should be snake cased and have a name that sounds like multiple results are expected.
    - name: list_by_bday
      where: birthdate < :birthdate    # you can use named parameters. This will create a ListByBDay(ctx, birthdate time.Time, startId int64, pageSize int) function
      order: birthdate
      descending: true
    - name: list_by_age
      where: age = :minAge OR age > :minAge
      order: created
      typeMapping:
        minAge: age
     # descending: true   # default is false
    - name: list_by_status
      where: status = :status
      order: created
  deletes: 
    - name: delete_older
      where: age > :age
circuitbreaker:
  timeoutSeconds: 20             # how long to wait while in open state before trying to go to half open state.
  consecutiveFailures: 4         # how many times to fail in an closed state before we swithc to open state
caching:
  type: memory # Currently supported is memory + pub/sub redis
  singleExpirationSeconds: 300  # timeout for local cache for single rows
  listExpirationSeconds: 60     # timeout for local cache for multiple rows aka list functions
  listInvalidation: flush       # potential values could be: expire, flush, epoch
  maxItemsCount: 100000         # max number of items in cache
```

2. Run DALForge:

`dalforge generate ./config ./internal/dal`

3. Use your generated code:
DALForge generates user.gen.go, user.sql, and interface files. You can immediately use the repository in your service layer:

```
repo := dal.NewUserRepository(dbProvider, redisCache, configProvider, cbSettings, telemetry)

// Instant cache-backed fetch!
user, err := repo.GetByEmail(ctx, "hello@example.com")
```

## 📖 Configuration Guide

Supported Column Types
DALForge maps YAML types to native Go and SQL types automatically:
int8, int16, int32, int64, float, varchar, text, bool, date, time, datetime, uid, json.

The operations Block
Define exactly what queries your repository needs. Unused operations are not generated, keeping your binary small.

write: Generates Create, CreateBulk, and Update.

delete / softDelete: Generates Delete (and HardDelete). Soft deletes automatically scope all gets and lists with deleted_at IS NULL.

gets: Generates single-item fetchers (e.g., GetByEmail). Fields must be marked unique: true.

getsBulk: Generates scatter-gather IN clause fetchers (e.g., GetByUids).

lists: Generates paginated SELECT queries with custom where clauses.

listsBulk: Generates IN clause lists. Supports mixing standard where scalars with a variadic whereIn parameter.

updatesBulk: Generates highly optimized bulk partial updates (e.g., UPDATE users SET status = ? WHERE uid IN (...)).

deletes: Generates custom bulk delete operations (e.g., DeleteExpired).

Telemetry & Circuit Breaking
DALForge strictly enforces safety. Bulk operations are hard-limited to 5000 items and automatically chunked into database queries of 500 parameters to prevent driver panics.

All generated operations report directly to a TelemetryProvider interface, allowing you to easily mock metrics in testing or bind them to Prometheus in production.

## 🧪 Testing
DALForge guarantees the validity of generated code. The generated templates themselves are heavily tested within this repository against real MySQL and Redis testcontainers.

Because the generator is tested, you do not need to write unit tests for the generated DAL code in your own projects. Simply mock the generated Interfaces in your service-layer tests.

To run the internal test suite:

`go test ./...`

## 🤝 Contributing
We welcome contributions! Whether it's a bug report, a new feature, or documentation improvements:

Fork the repository.

Create a new branch (git checkout -b feature/amazing-feature).

Commit your changes (git commit -m 'Add amazing feature').

Push to the branch (git push origin feature/amazing-feature).

Open a Pull Request.

Please ensure your code passes existing tests and includes new tests for added features.

## 📄 License
DALForge is open-source software released under the MIT License.