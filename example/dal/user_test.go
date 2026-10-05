// user_test.go
package dal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sony/gobreaker"
	"github.com/stretchr/testify/assert"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

var (
	mysqlContainer testcontainers.Container
	mainDbProvider *TestDBProvider
	dbProvider     *TestDBProvider
)

type TestDBProvider struct {
	connStr    string
	connection *sql.DB
}

func (p *TestDBProvider) GetDatabase(_ string, _ bool) (*sql.DB, error) {
	if p.connection == nil {
		return nil, fmt.Errorf("database is disconnected")
	}
	return p.connection, nil
}

func (p *TestDBProvider) AllDatabases(_ string, _ string) []*sql.DB {
	return nil
}
func (p *TestDBProvider) Connect() error {
	conn, err := sql.Open("mysql", p.connStr)
	if err != nil {
		return err
	}
	// ping connection
	if err = conn.Ping(); err != nil {
		return fmt.Errorf("failed to ping connection: %w", err)
	}

	p.connection = conn
	return nil
}
func (p *TestDBProvider) Disconnect() error {
	p.connection.Close()
	p.connection = nil
	return nil
}

func TestMain(m *testing.M) {
	ctx := context.Background()

	// Start MySQL container
	req := testcontainers.ContainerRequest{
		Image:        "mysql:8.0",
		ExposedPorts: []string{"3306/tcp"},
		Env: map[string]string{
			"MYSQL_ROOT_PASSWORD": "root",
			"MYSQL_DATABASE":      "test",
		},
		WaitingFor: wait.ForLog("port: 3306  MySQL Community Server"),
	}

	var err error
	mysqlContainer, err = testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		log.Fatalf("Failed to start container: %v", err)
	}
	defer mysqlContainer.Terminate(ctx)

	// Get connection details
	host, _ := mysqlContainer.Host(ctx)
	port, _ := mysqlContainer.MappedPort(ctx, "3306")

	connStr := fmt.Sprintf("root:root@tcp(%s:%s)/test?parseTime=true&multiStatements=true", host, port.Port())
	mainDbProvider = &TestDBProvider{connStr: connStr}
	err = mainDbProvider.Connect()
	if err != nil {
		log.Fatalf("failed connecting to mysql: %v", err)
	}

	// Run tests
	code := m.Run()
	os.Exit(code)
}

func setupTestDB(t *testing.T) {
	ResetTelemetry()
	registry := prometheus.NewRegistry()
	RegisterTelemetry(registry)

	ctx := context.Background()
	db, err := mainDbProvider.GetDatabase("", false)
	if err != nil {
		t.Fatal(err)
	}

	// Create test database
	dbName := fmt.Sprintf("test_%s", strings.ReplaceAll(uuid.New().String(), "-", "_"))
	_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", dbName))
	if err != nil {
		t.Fatal(err)
	}

	// Preserve original host:port from container
	originalConnStr := mainDbProvider.connStr
	parts := strings.Split(originalConnStr, "/")
	hostPortPart := parts[0] // "root:root@tcp(host:port)"

	connStr := fmt.Sprintf("%s/%s?parseTime=true&multiStatements=true", hostPortPart, dbName)
	dbProvider = &TestDBProvider{connStr: connStr}
	err = dbProvider.Connect()
	if err != nil {
		t.Fatal("failed connecting to database")
	}

	// Feed ALL necessary schemas for testing
	migrate(t, dbName, "user.sql", "match_reaction_counter.sql")
}

func teardownTestDB(t *testing.T) {
	ctx := context.Background()
	ResetTelemetry()

	// Extract host:port and database name
	parts := strings.Split(dbProvider.connStr, "/")
	hostPortPart := parts[0]
	dbName := parts[1]
	dbName = strings.Split(dbName, "?")[0]

	// Connect to root using container's host:port
	rootConnStr := fmt.Sprintf("%s/?parseTime=true&multiStatements=true", hostPortPart)
	db, err := sql.Open("mysql", rootConnStr)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	_, err = db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE %s", dbName))
	if err != nil {
		t.Fatal(err)
	}
}

func migrate(t *testing.T, dbName string, fileNames ...string) {
	db, err := dbProvider.GetDatabase("", false)
	if err != nil {
		t.Fatal(err)
	}

	for _, fileName := range fileNames {
		// Load SQL schema from file
		content, err := os.ReadFile(fileName)
		if err != nil {
			t.Fatalf("failed reading schema file %s: %v", fileName, err)
		}

		_, err = db.ExecContext(context.Background(), string(content))
		if err != nil {
			t.Fatalf("failed executing schema file %s: %v", fileName, err)
		}
	}
}

func TestUserCRUD(t *testing.T) {
	t.Run("CreateGetUpdateDelete", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		// Initialize DAL
		userDAL := NewUserRepository(
			dbProvider,
			nil, // cache provider (mock if needed)
			nil, // config provider (mock if needed)
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)

		ctx := context.Background()

		// Test Create
		initialMeta := json.RawMessage(`{"theme":"dark","notifications_enabled":true}`)
		newUser := &User{
			Age:       25,
			Email:     "test@example.com",
			Status:    Ptr("active"),
			Birthdate: Ptr(time.Now()),
			Meta:      &initialMeta,
		}

		created, err := userDAL.Create(ctx, newUser)
		assert.NoError(t, err)
		assert.NotZero(t, created.ID)
		assert.NotZero(t, created.Uid)

		// Assert that the create operation telemetry counter has increased.
		createCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "create"))
		assert.Equal(t, 1.0, createCounter, "Expected one create operation")

		// --- Test GetByID ---
		fetched, err := userDAL.GetByID(ctx, created.ID)
		assert.NoError(t, err)
		assert.Equal(t, created.ID, fetched.ID)
		assert.Equal(t, "test@example.com", fetched.Email)
		assert.NotNil(t, fetched.Meta)
		assert.JSONEq(t, `{"theme":"dark","notifications_enabled":true}`, string(*fetched.Meta))

		getByIDCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "get_by_id"))
		assert.Equal(t, 1.0, getByIDCounter, "Expected one get_by_id operation")

		cacheMissesCounter := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_id"))
		cacheHitsCounter := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_by_id"))
		assert.Equal(t, 0.0, cacheMissesCounter, "Expected zero cache miss for get_by_id operation")
		assert.Equal(t, 1.0, cacheHitsCounter, "Expected one cache hits for get_by_id operation")

		// --- Test Update ---
		newEmail := "updated@example.com"
		newMeta := json.RawMessage(`{"theme":"light","notifications_enabled":false}`)

		fetched.Email = newEmail
		fetched.Meta = &newMeta

		err = userDAL.Update(ctx, fetched)
		assert.NoError(t, err)

		updated, err := userDAL.GetByID(ctx, fetched.ID)
		assert.NoError(t, err)
		assert.Equal(t, newEmail, updated.Email)
		assert.NotNil(t, updated.Meta)
		assert.JSONEq(t, `{"theme":"light","notifications_enabled":false}`, string(*updated.Meta))

		updateCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "update"))
		assert.Equal(t, 1.0, updateCounter, "Expected one update operation")

		cacheMissesCounter = testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_id"))
		cacheHitsCounter = testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_by_id"))
		assert.Equal(t, 0.0, cacheMissesCounter, "Expected zero cache miss for get_by_id operation")
		assert.Equal(t, 3.0, cacheHitsCounter, "Expected three cache hits for get_by_id operation")

		// --- Test Delete ---
		err = userDAL.Delete(ctx, updated)
		assert.NoError(t, err)

		// Depending on your implementation, the delete operation's label may vary.
		// Here we assume that Delete increments dalOperationsTotalCounter with label "user", "delete".
		deleteCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "delete"))
		assert.Equal(t, 1.0, deleteCounter, "Expected one delete operation")

		// Verify deletion
		_, err = userDAL.GetByID(ctx, updated.ID)
		assert.ErrorIs(t, err, ErrNotFound)
	})
}

func TestUserCreateBulk(t *testing.T) {
	t.Run("TestUserCreateBulk", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		// Initialize DAL
		userDAL := NewUserRepository(
			dbProvider,
			nil, // cache provider (mock if needed)
			nil, // config provider (mock if needed)
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)

		ctx := context.Background()

		const numEntries = 5000
		var users []*User

		for i := 1; i <= numEntries; i++ {
			// Test Create
			metaData := json.RawMessage(fmt.Sprintf(`{"index": %d}`, i))
			newUser := &User{
				Age:       25,
				Email:     fmt.Sprintf("test_%02d@example.com", i),
				Status:    Ptr("active"),
				Birthdate: Ptr(time.Now()),
				Meta:      &metaData,
			}
			users = append(users, newUser)
		}

		users, err := userDAL.CreateBulk(ctx, users)
		assert.NoError(t, err)
		assert.Equal(t, numEntries, len(users))

		// Assert that the create operation telemetry counter has increased.
		createCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "create_bulk"))
		assert.Equal(t, 1.0, createCounter, "Expected one create_bulk operation")

		// assert we got all the IDs
		for _, user := range users {
			assert.Greater(t, user.ID, int64(0))
			assert.NotZero(t, user.Uid)
		}

		// --- Test GetByID ---
		fetched, err := userDAL.GetByID(ctx, users[0].ID)
		assert.NoError(t, err)
		assert.Equal(t, users[0].ID, fetched.ID)
		assert.Equal(t, "test_01@example.com", fetched.Email)
		assert.NotNil(t, fetched.Meta)
		assert.JSONEq(t, `{"index": 1}`, string(*fetched.Meta))
	})
}

// DefaultConfigProvider is an implementation of ConfigProvider that
// always returns true for both BlockedReads and BlockedWrites.
type AlwaysBlockingConfigProvider struct{}

// BlockedReads always returns true.
func (d AlwaysBlockingConfigProvider) BlockedReads(entityName string) bool {
	return true
}

// BlockedWrites always returns true.
func (d AlwaysBlockingConfigProvider) BlockedWrites(entityName string) bool {
	return true
}

func TestUserBlockedReadsAndWrites(t *testing.T) {
	t.Run("TestUserBlockedReadsAndWrites", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		configProvider := AlwaysBlockingConfigProvider{}
		// Initialize DAL
		userDAL := NewUserRepository(
			dbProvider,
			nil,            // cache provider (mock if needed)
			configProvider, // config provider (mock if needed)
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)

		ctx := context.Background()

		// Test Create
		newUser := &User{
			Age:       25,
			Email:     "test@example.com",
			Status:    Ptr("active"),
			Birthdate: Ptr(time.Now()),
		}

		_, err := userDAL.Create(ctx, newUser)
		assert.ErrorIs(t, err, ErrOperationBlocked)

		_, err = userDAL.GetByID(ctx, 1)
		assert.ErrorIs(t, err, ErrOperationBlocked)

		err = userDAL.Update(ctx, newUser)
		assert.ErrorIs(t, err, ErrOperationBlocked)

		err = userDAL.Delete(ctx, newUser)
		assert.ErrorIs(t, err, ErrOperationBlocked)
	})
}

func TestUserGetEmail(t *testing.T) {
	t.Run("TestUserGetEmail", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		// Initialize DAL
		userDAL := NewUserRepository(
			dbProvider,
			nil, // cache provider (mock if needed)
			nil, // config provider (mock if needed)
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)

		ctx := context.Background()

		// Test Create
		newUser := &User{
			Age:       25,
			Email:     "test@example.com",
			Status:    Ptr("active"),
			Birthdate: Ptr(time.Now()),
		}

		created, err := userDAL.Create(ctx, newUser)
		assert.NoError(t, err)
		assert.NotZero(t, created.ID)

		// Assert that the create operation telemetry counter has increased.
		createCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "create"))
		assert.Equal(t, 1.0, createCounter, "Expected one create operation")

		// --- Test GetByEmail
		fetched, err := userDAL.GetByEmail(ctx, "test@example.com")
		assert.NoError(t, err)
		assert.Equal(t, created.ID, fetched.ID)
		assert.Equal(t, "test@example.com", fetched.Email)

		getByEmailCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "get_by_email"))
		assert.Equal(t, 1.0, getByEmailCounter, "Expected one get_by_email operation")

		// no caching of get_by_id should happen here.
		cacheIDMissesCounter := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_id"))
		cacheIDHitsCounter := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_by_id"))
		assert.Equal(t, 0.0, cacheIDMissesCounter, "Expected zero cache miss for get_by_id operation")
		assert.Equal(t, 0.0, cacheIDHitsCounter, "Expected zero cache hits for get_by_id operation")

		cacheMissesCounter := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_email"))
		cacheHitsCounter := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_by_email"))
		assert.Equal(t, 1.0, cacheMissesCounter, "Expected one cache miss for get_by_email operation")
		assert.Equal(t, 0.0, cacheHitsCounter, "Expected zero cache hits for get_by_email operation")

		// Test missing entry
		concreteDAL := userDAL.(*userRepository)
		_, err = concreteDAL.getByEmail(ctx, "nonexisting@example.com")
		assert.ErrorIs(t, err, ErrNotFound)

		// Test using cache
		fetched, err = userDAL.GetByEmail(ctx, "test@example.com")
		assert.NoError(t, err)
		assert.Equal(t, created.ID, fetched.ID)
		assert.Equal(t, "test@example.com", fetched.Email)

		getByEmailCounter = testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "get_by_email"))
		assert.Equal(t, 2.0, getByEmailCounter, "Expected two get_by_email operation")

		cacheMissesCounter = testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_email"))
		cacheHitsCounter = testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_by_email"))
		assert.Equal(t, 1.0, cacheMissesCounter, "Expected one cache miss for get_by_email operation")
		assert.Equal(t, 1.0, cacheHitsCounter, "Expected one cache hits for get_by_email operation")

		// caching now has id -> entity mapping so we should get one hit
		cacheIDMissesCounter = testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_id"))
		cacheIDHitsCounter = testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_by_id"))
		assert.Equal(t, 0.0, cacheIDMissesCounter, "Expected zero cache miss for get_by_id operation")
		assert.Equal(t, 1.0, cacheIDHitsCounter, "Expected one cache hits for get_by_id operation")
	})
}

func TestListById(t *testing.T) {
	t.Run("TestListById", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		// Initialize DAL
		userDAL := NewUserRepository(
			dbProvider,
			nil, // cache provider (mock if needed)
			nil, // config provider (mock if needed)
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)

		ctx := context.Background()

		const numEntries = 100
		// lets create some entries
		for i := 1; i <= numEntries; i++ {
			// Test Create
			newUser := &User{
				Age:       25,
				Email:     fmt.Sprintf("test_%d@example.com", i),
				Status:    Ptr("active"),
				Birthdate: Ptr(time.Now()),
			}

			created, err := userDAL.Create(ctx, newUser)
			assert.NoError(t, err)
			assert.NotZero(t, created.ID)

			// Assert that the create operation telemetry counter has increased.
			createCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "create"))
			assert.Equal(t, float64(i), createCounter, "Expected enough create operations")
		}

		// test count
		count, err := userDAL.CountListById(ctx)
		assert.NoError(t, err)
		assert.Equal(t, int64(numEntries), count)

		// assert it is cached by running one more time.
		count, err = userDAL.CountListById(ctx)
		assert.NoError(t, err)
		assert.Equal(t, int64(numEntries), count)

		countListByIDCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "count_list_by_id"))
		assert.Equal(t, 2.0, countListByIDCounter, "Expected count_list_by_id counter to equal 2 after two calls")

		countCacheHitCounter := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "count_list_by_id"))
		assert.Equal(t, 1.0, countCacheHitCounter, "Expected one cache hit on count_list_by_id counter")

		// Test ListById with pagination.
		const pageSize = 10

		// First page: startID = 0 should return the first page.
		usersPage1, err := userDAL.ListById(ctx, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, usersPage1, pageSize, "Expected first page to have exactly %d entries", pageSize)
		// Verify that the IDs are in ascending order.
		for i := 1; i < len(usersPage1); i++ {
			assert.True(t, usersPage1[i].ID > usersPage1[i-1].ID, "Expected IDs to be in ascending order")
		}

		// Next page: use the last ID from page1 as the starting point.
		lastID := usersPage1[len(usersPage1)-1].ID
		usersPage2, err := userDAL.ListById(ctx, lastID, pageSize)
		assert.NoError(t, err)
		// Expect page2 to have pageSize entries if there are enough users.
		assert.Len(t, usersPage2, pageSize, "Expected second page to have exactly %d entries", pageSize)
		// The first entry in page2 should have an ID greater than lastID.
		assert.True(t, usersPage2[0].ID > lastID, "Expected first user ID in page2 to be greater than last ID from page1")

		// Assert telemetry for ListById.
		// Two separate ListById calls should have incremented the counter by 2.
		listByIDCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "list_by_id"))
		assert.Equal(t, 2.0, listByIDCounter, "Expected list_by_id counter to equal 2 after two calls")

	})
}

func TestListByIdCachingAndInvalidation(t *testing.T) {
	t.Run("TestListByIdCachingAndInvalidation", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		// Initialize the DAL with default providers (will use NoopCacheProvider and DefaultConfigProvider if nil).
		userDAL := NewUserRepository(
			dbProvider,
			nil, // cache provider (default if nil)
			nil, // config provider (default if nil)
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)
		ctx := context.Background()
		const numEntries = 50
		const pageSize = 10

		// Insert a set of users.
		for i := 1; i <= numEntries; i++ {
			newUser := &User{
				Age:       25,
				Email:     fmt.Sprintf("cache_test_%d@example.com", i),
				Status:    Ptr("active"),
				Birthdate: Ptr(time.Now()),
			}
			created, err := userDAL.Create(ctx, newUser)
			assert.NoError(t, err)
			assert.NotZero(t, created.ID)
		}

		// First call to ListById: this call should load data from the DB and set the cache.
		usersPage1, err := userDAL.ListById(ctx, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, usersPage1, pageSize, "Expected first page to have %d entries", pageSize)

		// Record cache hit and miss counters after first call.
		hitsAfterFirst := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "list_by_id"))
		missesAfterFirst := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "list_by_id"))

		// Second call to ListById with the same parameters: should use the list cache and then individual item cache.
		usersPage1Cached, err := userDAL.ListById(ctx, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, usersPage1Cached, pageSize, "Expected first page (cached) to have %d entries", pageSize)

		// Get updated telemetry counters.
		hitsAfterSecond := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "list_by_id"))
		missesAfterSecond := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "list_by_id"))

		// Expect that cache hits have increased on the second call.
		assert.Greater(t, hitsAfterSecond, hitsAfterFirst, "Expected cache hits to increase on second call")
		// Expect no new cache misses if items were found in cache.
		assert.Equal(t, missesAfterFirst, missesAfterSecond, "Expected cache misses to remain the same on second call")

		// Flush the list cache to simulate cache invalidation.
		userDAL.FlushListCache()

		// Third call to ListById: since list cache was flushed, the DAL should fall back to DB,
		// leading to new cache misses for the list retrieval.
		usersPage1AfterFlush, err := userDAL.ListById(ctx, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, usersPage1AfterFlush, pageSize, "Expected first page to have %d entries after cache flush", pageSize)

		// After flush, cache misses should increase.
		missesAfterFlush := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "list_by_id"))
		assert.Greater(t, missesAfterFlush, missesAfterSecond, "Expected cache misses to increase after list cache flush")
	})
}

func TestListByBday(t *testing.T) {
	t.Run("TestListByBday", func(t *testing.T) {
		// Set up a fresh test database.
		setupTestDB(t)
		defer teardownTestDB(t)

		// Initialize the DAL with default cache and config providers.
		userDAL := NewUserRepository(
			dbProvider,
			nil, // cache provider (defaults to NoopCacheProvider if nil)
			nil, // config provider (defaults to DefaultConfigProvider if nil)
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)
		ctx := context.Background()

		// Insert 12 users, one per month in 1990.
		for month := 1; month <= 12; month++ {
			birthdate := time.Date(1990, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
			newUser := &User{
				Age:       30,
				Email:     fmt.Sprintf("user_%02d@example.com", month),
				Status:    Ptr("active"),
				Birthdate: &birthdate,
			}
			created, err := userDAL.Create(ctx, newUser)
			assert.NoError(t, err)
			assert.NotZero(t, created.ID)
		}

		// Test ListByBday for March 1, 1990.
		bdayToTest := time.Date(1990, time.May, 1, 0, 0, 0, 0, time.UTC)
		nullBday := Ptr(bdayToTest)
		pageSize := 10

		count, err := userDAL.CountListByBday(ctx, nullBday)
		assert.NoError(t, err)
		assert.Equal(t, int64(4), count)

		// First call: should load from DB and populate the cache.
		results, err := userDAL.ListByBday(ctx, nullBday, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, results, 4, "Expected 4 users for birthdate %v", bdayToTest)
		assert.Equal(t, "user_04@example.com", results[0].Email)

		// Second call: should hit the cache.
		resultsCached, err := userDAL.ListByBday(ctx, nullBday, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, resultsCached, 4, "Expected 4 users from cache for birthdate %v", bdayToTest)
		assert.Equal(t, "user_04@example.com", resultsCached[0].Email)

		// Verify that the telemetry counter for "list_by_bday" has incremented by 2.
		listByBdayCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "list_by_bday"))
		assert.Equal(t, 2.0, listByBdayCounter, "Expected list_by_bday counter to equal 2 after two calls")
	})
}

func TestOptimisticLocking(t *testing.T) {
	t.Run("TestOptimisticLocking", func(t *testing.T) {
		// Set up a fresh test database.
		setupTestDB(t)
		defer teardownTestDB(t)

		// Initialize DAL with default providers.
		userDAL := NewUserRepository(
			dbProvider,
			nil, // default cache provider
			nil, // default config provider
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)
		ctx := context.Background()

		// Create a user.
		newUser := &User{
			Age:       25,
			Email:     "optimistic@example.com",
			Status:    Ptr("active"),
			Birthdate: Ptr(time.Now()),
		}
		created, err := userDAL.Create(ctx, newUser)
		assert.NoError(t, err)
		assert.NotZero(t, created.ID)

		// Simulate an external update that bumps the version.
		// We update the record directly so that the version in the database is incremented.
		db, err := dbProvider.GetDatabase("user", true)
		assert.NoError(t, err)
		_, err = db.ExecContext(ctx, "UPDATE users SET version = version + 1 WHERE id = ?", created.ID)
		assert.NoError(t, err)

		// Now try to update the user using the stale version information from 'created'.
		// Change a field.
		created.Email = "updated@example.com"

		// The update should use the stale version (e.g. X) while the DB has version (X+1), so no rows are affected.
		err = userDAL.Update(ctx, created)
		assert.ErrorIs(t, err, ErrNotFound, "Expected update to fail due to optimistic locking conflict")

		// new version should be higher than created
		retrieved, err := userDAL.GetByID(ctx, created.ID)
		assert.NoError(t, err)
		assert.Greater(t, retrieved.Version, created.Version)
	})
}

func TestUserSoftAndHardDelete(t *testing.T) {
	t.Run("TestSoftAndHardDelete", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		// Initialize DAL
		userDAL := NewUserRepository(
			dbProvider,
			nil, // default cache provider
			nil, // default config provider
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)

		ctx := context.Background()

		// 1. Create a "Ghost" User
		originalEmail := "ghost_account@example.com"
		ghostUser := &User{
			Age:       25,
			Email:     originalEmail,
			Status:    Ptr("active"),
			Birthdate: Ptr(time.Now()),
		}

		createdGhost, err := userDAL.Create(ctx, ghostUser)
		assert.NoError(t, err)
		assert.NotZero(t, createdGhost.ID)

		// 2. Perform the Soft Delete
		err = userDAL.Delete(ctx, createdGhost)
		assert.NoError(t, err)

		// 3. Verify DAL scoping (User is invisible to GetByID)
		_, err = userDAL.GetByID(ctx, createdGhost.ID)
		assert.ErrorIs(t, err, ErrNotFound, "Soft deleted user should not be found by GetByID")

		// 4. Verify raw Database state (Row exists, deleted_at is set, unique fields scrambled)
		db, err := dbProvider.GetDatabase("user", false)
		assert.NoError(t, err)

		var deletedAt sql.NullTime
		var scrambledEmail, scrambledUid string

		// Bypassing DAL to check raw table state
		err = db.QueryRow("SELECT deleted_at, email, uid FROM users WHERE id = ?", createdGhost.ID).
			Scan(&deletedAt, &scrambledEmail, &scrambledUid)

		assert.NoError(t, err)
		assert.True(t, deletedAt.Valid, "deleted_at timestamp should be set in the database")
		assert.Contains(t, scrambledEmail, "-del-", "Email should be scrambled to release unique constraint")
		assert.Contains(t, scrambledUid, "-del-", "UID should be scrambled to release unique constraint")

		// 5. Verify Re-registration (Unique constraint successfully bypassed)
		returningUser := &User{
			Age:       30,
			Email:     originalEmail, // Using the exact same email!
			Status:    Ptr("active"),
			Birthdate: Ptr(time.Now()),
		}

		createdReturning, err := userDAL.Create(ctx, returningUser)
		assert.NoError(t, err, "Should be able to create a new user with the original email")
		assert.NotEqual(t, createdGhost.ID, createdReturning.ID)

		// 6. Perform the Hard Delete on the original ghost account
		err = userDAL.HardDelete(ctx, createdGhost)
		assert.NoError(t, err)

		// 7. Verify raw Database state (Row is completely purged)
		var count int
		err = db.QueryRow("SELECT count(*) FROM users WHERE id = ?", createdGhost.ID).Scan(&count)
		assert.NoError(t, err)
		assert.Equal(t, 0, count, "Row should be completely removed from the database after HardDelete")
	})
}

func TestCustomBulkDeletes(t *testing.T) {
	t.Run("TestBulkSoftAndHardDeletes", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		userDAL := NewUserRepository(
			dbProvider,
			nil, nil, gobreaker.Settings{}, PrometheusTelemetryProvider{},
		)
		ctx := context.Background()

		// 1. Seed database with users of varying ages
		ages := []int8{20, 25, 30, 35, 40}
		for _, age := range ages {
			_, err := userDAL.Create(ctx, &User{
				Age:       age,
				Email:     fmt.Sprintf("user_age_%d@example.com", age),
				Status:    Ptr("active"),
				Birthdate: Ptr(time.Now()),
			})
			assert.NoError(t, err)
		}

		// 2. Test Soft Bulk Delete (DeleteOlder where age > 28)
		// Expected to affect ages 30, 35, 40 (3 rows)
		rowsAffected, err := userDAL.DeleteOlder(ctx, 28, 5000)
		assert.NoError(t, err)
		assert.Equal(t, int64(3), rowsAffected)

		// Verify DAL scoping (older users are hidden)
		_, err = userDAL.GetByEmail(ctx, "user_age_35@example.com")
		assert.ErrorIs(t, err, ErrNotFound)

		// Verify raw Database state for a soft-deleted record
		db, _ := dbProvider.GetDatabase("user", false)
		var deletedAt sql.NullTime
		var scrambledEmail string
		err = db.QueryRow("SELECT deleted_at, email FROM users WHERE age = 35").Scan(&deletedAt, &scrambledEmail)
		assert.NoError(t, err)
		assert.True(t, deletedAt.Valid, "deleted_at should be populated")
		assert.Contains(t, scrambledEmail, "-del-", "email should be scrambled")

		// Verify raw Database state for an unaffected record
		err = db.QueryRow("SELECT deleted_at, email FROM users WHERE age = 25").Scan(&deletedAt, &scrambledEmail)
		assert.NoError(t, err)
		assert.False(t, deletedAt.Valid, "younger users should not be deleted")
		assert.Equal(t, "user_age_25@example.com", scrambledEmail)

		// 3. Test Zero-Row Optimization
		// Running the same delete again should yield 0 affected rows because of the `deleted_at IS NULL` scope
		rowsAffected, err = userDAL.DeleteOlder(ctx, 28, 5000)
		assert.NoError(t, err)
		assert.Equal(t, int64(0), rowsAffected, "Already deleted records should be ignored")

		// 4. Test Hard Bulk Delete (HardDeleteOlder where age > 22)
		// Expected to affect age 25 (active) AND ages 30, 35, 40 (soft-deleted).
		// Hard deletes bypass the `deleted_at IS NULL` check.
		rowsAffected, err = userDAL.HardDeleteOlder(ctx, 22, 5000)
		assert.NoError(t, err)
		assert.Equal(t, int64(4), rowsAffected)

		// Verify raw Database state (only age 20 should remain in the table entirely)
		var count int
		err = db.QueryRow("SELECT count(*) FROM users").Scan(&count)
		assert.NoError(t, err)
		assert.Equal(t, 1, count, "Only the 20-year-old user should remain in the table")
	})
}

func TestUserBulkGets(t *testing.T) {
	t.Run("TestBulkGetOperations", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		userDAL := NewUserRepository(
			dbProvider,
			nil, nil, gobreaker.Settings{}, PrometheusTelemetryProvider{},
		)
		ctx := context.Background()

		// 1. Create a few users
		var createdUsers []*User
		var uids []string
		var ids []int64

		for i := 1; i <= 3; i++ {
			u, err := userDAL.Create(ctx, &User{
				Age:       int8(20 + i),
				Email:     fmt.Sprintf("bulkget_%d@example.com", i),
				Status:    Ptr("active"),
				Birthdate: Ptr(time.Now()),
			})
			assert.NoError(t, err)
			createdUsers = append(createdUsers, u)
			uids = append(uids, u.Uid)
			ids = append(ids, u.ID)
		}

		// 2. Test GetByUids
		fetchedByUids, err := userDAL.GetByUids(ctx, uids)
		assert.NoError(t, err)
		assert.Len(t, fetchedByUids, 3)

		// 3. Test Caching for GetByUids
		// First call should have resulted in cache misses and DB requests.
		// Let's call it again to hit the cache.
		_, err = userDAL.GetByUids(ctx, uids)
		assert.NoError(t, err)

		hits := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_bulk_by_uid"))
		assert.Equal(t, 1.0, hits, "Expected 1 cache hit (for the entire bulk operation) on the second get by uid")

		// 4. Test GetByIds
		fetchedByIds, err := userDAL.GetByIds(ctx, ids)
		assert.NoError(t, err)
		assert.Len(t, fetchedByIds, 3)

		// 5. Test Hard Limits
		overLimitUids := make([]string, 5001)
		_, err = userDAL.GetByUids(ctx, overLimitUids)
		assert.ErrorContains(t, err, "exceeds maximum limit of 5000 items")

		overLimitIds := make([]int64, 5001)
		_, err = userDAL.GetByIds(ctx, overLimitIds)
		assert.ErrorContains(t, err, "exceeds maximum limit of 5000 items")

		// 6. Test Empty Slices
		emptyRes, err := userDAL.GetByUids(ctx, []string{})
		assert.NoError(t, err)
		assert.Nil(t, emptyRes)
	})
}

func TestUserBulkUpdates(t *testing.T) {
	t.Run("TestBulkUpdateOperations", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		userDAL := NewUserRepository(
			dbProvider,
			nil, nil, gobreaker.Settings{}, PrometheusTelemetryProvider{},
		)
		ctx := context.Background()

		// 1. Create a few users
		var uidsToUpdate []string
		var idsToUpdate []int64
		var controlUser *User

		for i := 1; i <= 3; i++ {
			u, err := userDAL.Create(ctx, &User{
				Age:       25,
				Email:     fmt.Sprintf("bulkupdate_%d@example.com", i),
				Status:    Ptr("active"),
				Birthdate: Ptr(time.Now()),
			})
			assert.NoError(t, err)

			if i < 3 {
				uidsToUpdate = append(uidsToUpdate, u.Uid)
				idsToUpdate = append(idsToUpdate, u.ID)
			} else {
				// Third user is our control to ensure we don't accidentally update everyone
				controlUser = u
			}
		}

		// 2. Pre-fetch to warm up the single-item cache specifically for these queries
		_, err := userDAL.GetByUid(ctx, uidsToUpdate[0])
		assert.NoError(t, err)
		_, err = userDAL.GetByUid(ctx, controlUser.Uid)
		assert.NoError(t, err)

		// 3. Test UpdateStatusByUids
		newStatus := "suspended"
		err = userDAL.UpdateStatusByUids(ctx, &newStatus, uidsToUpdate)
		assert.NoError(t, err)

		// 4. Verify cache was flushed BEFORE we re-fetch the data
		missesBefore := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_uid"))

		// Fetch u1 to verify the update worked (this triggers the cache miss because of the flush)
		u1, _ := userDAL.GetByUid(ctx, uidsToUpdate[0])
		assert.Equal(t, "suspended", *u1.Status)

		missesAfter := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_uid"))
		assert.Greater(t, missesAfter, missesBefore, "Expected cache miss after bulk update flushed the cache")

		// Verify control user was NOT updated
		u3, _ := userDAL.GetByUid(ctx, controlUser.Uid)
		assert.Equal(t, "active", *u3.Status)

		// 5. Test UpdateAgeByIds
		newAge := int8(99)
		err = userDAL.UpdateAgeByIds(ctx, newAge, idsToUpdate)
		assert.NoError(t, err)

		u2, _ := userDAL.GetByID(ctx, idsToUpdate[1])
		assert.Equal(t, int8(99), u2.Age)

		// 6. Test Hard Limits
		overLimitUids := make([]string, 5001)
		err = userDAL.UpdateStatusByUids(ctx, &newStatus, overLimitUids)
		assert.ErrorContains(t, err, "exceeds maximum limit of 5000 items")

		// 7. Test Empty Slices
		err = userDAL.UpdateAgeByIds(ctx, 30, []int64{})
		assert.NoError(t, err)
	})
}

func TestUserUpsertBulk(t *testing.T) {
	t.Run("TestUpsertUserSnapshots", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		// Instantiate the repository behind its interface to mirror the Service layer
		userDAL := NewUserRepository(
			dbProvider,
			nil, // default cache provider
			nil, // default config provider
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)
		ctx := context.Background()

		// 1. Initial Insert using Upsert
		meta1 := json.RawMessage(`{"theme":"dark"}`)
		meta2 := json.RawMessage(`{"theme":"light"}`)

		uid1 := "user_upsert_1"
		uid2 := "user_upsert_2"

		users := []*User{
			{
				Age:       25,
				Email:     "upsert1@example.com",
				Status:    Ptr("active"),
				Uid:       uid1,
				Meta:      &meta1,
				Birthdate: Ptr(time.Now()),
			},
			{
				Age:       30,
				Email:     "upsert2@example.com",
				Status:    Ptr("pending"),
				Uid:       uid2,
				Meta:      &meta2,
				Birthdate: Ptr(time.Now()),
			},
		}

		err := userDAL.UpsertUserSnapshots(ctx, users)
		assert.NoError(t, err)

		// Verify telemetry for the bulk operation
		opsCounter := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "upsert_bulk_upsert_user_snapshots"))
		assert.Equal(t, 1.0, opsCounter, "Expected one upsert bulk operation")

		// Fetch the newly created records to get their IDs and warm the local cache
		fetched1, err := userDAL.GetByUid(ctx, uid1)
		assert.NoError(t, err)
		fetched2, err := userDAL.GetByUid(ctx, uid2)
		assert.NoError(t, err)

		// Verify they are cached correctly
		hitsBefore := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_by_id"))
		_, err = userDAL.GetByID(ctx, fetched1.ID)
		assert.NoError(t, err)
		hitsAfter := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "get_by_id"))
		assert.Greater(t, hitsAfter, hitsBefore, "Expected a cache hit after fetching via GetByUid")

		// 2. Update existing records via Upsert (Absolute Snapshot Sync)
		newMeta := json.RawMessage(`{"theme":"system"}`)

		// We modify the statuses and metadata for our snapshot update
		fetched1.Status = Ptr("banned")
		fetched1.Meta = &newMeta

		fetched2.Status = Ptr("active")

		err = userDAL.UpsertUserSnapshots(ctx, []*User{fetched1, fetched2})
		assert.NoError(t, err)

		// Verify cache flush behavior
		// Since Upsert bypasses individual item tracking, it must trigger a global flush.
		missesBefore := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_id"))

		updated1, err := userDAL.GetByID(ctx, fetched1.ID)
		assert.NoError(t, err)

		missesAfter := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_id"))
		assert.Greater(t, missesAfter, missesBefore, "Cache should have been flushed by the upsert, resulting in a cache miss")

		// Verify fields were updated correctly
		assert.Equal(t, "banned", *updated1.Status)
		assert.JSONEq(t, `{"theme":"system"}`, string(*updated1.Meta))

		// Ensure internal versioning is correctly incremented by the ON DUPLICATE KEY UPDATE clause
		assert.Greater(t, updated1.Version, fetched1.Version, "Version should be incremented upon upsert")

		updated2, _ := userDAL.GetByID(ctx, fetched2.ID)
		assert.Equal(t, "active", *updated2.Status)

		// 3. Test Hard Limits
		// Protects the infrastructure from massive memory spikes and runaway queries
		overLimit := make([]*User, 5001)
		err = userDAL.UpsertUserSnapshots(ctx, overLimit)
		assert.ErrorContains(t, err, "exceeds maximum limit of 5000 items")
	})
}

func TestListByIdScatterGatherHealing(t *testing.T) {
	t.Run("TestScatterGatherAndZombieHealing", func(t *testing.T) {
		setupTestDB(t)
		defer teardownTestDB(t)

		userDAL := NewUserRepository(
			dbProvider,
			nil,
			nil,
			gobreaker.Settings{},
			PrometheusTelemetryProvider{},
		)
		ctx := context.Background()

		// 1. Seed 5 users
		var createdUsers []*User
		for i := 1; i <= 5; i++ {
			u, err := userDAL.Create(ctx, &User{
				Age:       25,
				Email:     fmt.Sprintf("scatter_%d@example.com", i),
				Status:    Ptr("active"),
				Birthdate: Ptr(time.Now()),
			})
			assert.NoError(t, err)
			createdUsers = append(createdUsers, u)
		}

		// 2. Warm up the list cache
		pageSize := 10
		page1, err := userDAL.ListById(ctx, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, page1, 5)

		// Reset telemetry to accurately track the scatter-gather metrics
		ResetTelemetry()

		// 3. SCATTER-GATHER TEST
		// Evict one item from the single-item cache to trigger the partial miss cascade.
		evictedUser := createdUsers[2]
		concreteDAL := userDAL.(*userRepository)
		cacheKey := concreteDAL.getCacheKey(evictedUser.ID)
		concreteDAL.cache.Delete(cacheKey)

		// Call ListById again. It should hit the list cache, miss on the single item, and selectively call GetByIds.
		page1Recovered, err := userDAL.ListById(ctx, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, page1Recovered, 5, "Expected all 5 users to be returned in order")

		// Verify telemetry: We expect a list cache hit, a single item cache miss, and exactly one bulk fetch.
		listHits := testutil.ToFloat64(dalCacheHitsCounter.WithLabelValues("user", "list_by_id"))
		assert.Equal(t, 1.0, listHits, "Expected list cache hit")

		itemMisses := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "get_by_id"))
		// ARCHITECTURE: We expect exactly 2 misses.
		// 1 miss during the initial ListById array iteration.
		// 1 miss inside the delegated GetByIds generic bulk fetcher verifying local memory.
		assert.Equal(t, 2.0, itemMisses, "Expected exactly 2 item cache misses due to scatter-gather delegation")

		bulkGets := testutil.ToFloat64(dalOperationsTotalCounter.WithLabelValues("user", "get_bulk_by_id"))
		assert.Equal(t, 1.0, bulkGets, "Expected GetByIds to be called once for the missing item")

		// 4. ZOMBIE ID HEALING TEST
		ResetTelemetry()
		zombieUser := createdUsers[4]

		// Hard delete a user directly from DB to bypass DAL cache invalidation (simulating an external data wipe or race condition)
		db, _ := dbProvider.GetDatabase("user", true)
		_, err = db.ExecContext(ctx, "DELETE FROM users WHERE id = ?", zombieUser.ID)
		assert.NoError(t, err)

		// Remove it from the local item cache so the scatter-gather is forced to look for it in the DB
		concreteDAL.cache.Delete(concreteDAL.getCacheKey(zombieUser.ID))

		// Call ListById. It will try to scatter-gather the zombie ID, fail to find it, compact the list, and heal the list cache.
		page1Compacted, err := userDAL.ListById(ctx, 0, pageSize)
		assert.NoError(t, err)
		assert.Len(t, page1Compacted, 4, "Expected list to be compacted to 4 items")

		// 5. CACHE HEALING VERIFICATION
		ResetTelemetry()

		// Call ListById one last time to prove the list cache was successfully deleted during compaction
		_, err = userDAL.ListById(ctx, 0, pageSize)
		assert.NoError(t, err)

		// Because the previous call healed (deleted) the list cache, this MUST result in a list cache miss
		listMissesAfterHeal := testutil.ToFloat64(dalCacheMissesCounter.WithLabelValues("user", "list_by_id"))
		assert.Equal(t, 1.0, listMissesAfterHeal, "Expected list cache miss because the zombie healing should have deleted the stale list key")
	})
}
