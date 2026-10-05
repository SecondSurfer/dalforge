package generator

import (
	"strings"
	"testing"
)

func TestValidateEntityConfig_Valid(t *testing.T) {
	validConfig := EntityConfig{
		Name:    "user", // snake_case, >3 chars
		Version: "v1",   // non-empty and valid format
		Columns: map[string]Column{
			"id":          {Type: "int64", AllowNull: false, Unique: true},
			"public_id":   {Type: "uid", Prefix: "user", AllowNull: false, Unique: true},
			"email":       {Type: "varchar", AllowNull: false, Unique: true},
			"preferences": {Type: "json", AllowNull: true, Unique: false},
			"status":      {Type: "varchar", AllowNull: false, Unique: false},
			"score":       {Type: "int32", AllowNull: false, Unique: false}, // Valid column for buffer
			"created":     {Type: "datetime", AllowNull: false, Unique: false},
			"updated":     {Type: "datetime", AllowNull: false, Unique: false},
		},
		Operations: OperationConfig{
			Gets: []string{"id", "email", "public_id"},
			Lists: []ListConfig{
				{
					Name:       "user_list",
					Where:      "email = :emailParam AND public_id = :pubId",
					Order:      "created",
					Descending: false,
					TypeMapping: map[string]string{
						"emailParam": "email",
						"pubId":      "public_id",
					},
				},
			},
			Deletes: []DeleteConfig{
				{
					Name:  "delete_expired",
					Where: "created < :cutoff",
					TypeMapping: map[string]string{
						"cutoff": "created",
					},
				},
			},
			Write:  true,
			Delete: true,
			ListsBulk: []ListBulkConfig{
				{
					Name:    "by_statuses",
					WhereIn: "status",
				},
			},
			Plucks: []PluckConfig{
				{
					Name:   "pluck_emails_by_status",
					Column: "email",
					Where:  "status = :status",
				},
				{
					Name:   "pluck_ids_by_creation",
					Column: "id",
					Where:  "created > :cutoff",
					TypeMapping: map[string]string{
						"cutoff": "created",
					},
				},
			},
			UpsertsBulk: []UpsertBulkConfig{
				{
					Name:           "upsert_user_scores",
					ConflictTarget: "public_id",
					UpdateColumns:  []string{"score"},
					Increment:      true,
				},
			},
		},
		BufferedCounters: []BufferedCounterConfig{
			{
				Name:                 "increment_score",
				Column:               "score",
				GroupBy:              "public_id",
				FlushIntervalSeconds: 300,
			},
		},
		CircuitBreaker: CircuitBreakerConfig{
			TimeoutSeconds:      30,
			ConsecutiveFailures: 5,
		},
		Caching: CachingConfig{
			Type:                    "memory",
			SingleExpirationSeconds: 60,
			ListExpirationSeconds:   60,
			ListInvalidation:        "expire", // FIX: Reverted to 'expire' to pass existing validator logic
			MaxItemsCount:           20,
		},
	}

	err := ValidateEntityConfig(validConfig)
	if err != nil {
		t.Errorf("Expected valid config to pass validation, got error: %v", err)
	}
}

func TestValidateEntityConfig_Invalid(t *testing.T) {
	invalidConfig := EntityConfig{
		Name:    "User", // not snake_case
		Version: "",     // empty version (error)
		Columns: map[string]Column{
			"id":             {Type: "int64", AllowNull: false, Unique: true},
			"email":          {Type: "varchar", AllowNull: false, Unique: false},                   // error: used in gets but not unique
			"firstName":      {Type: "varchar", AllowNull: false, Unique: false},                   // error: not snake_case
			"invalid":        {Type: "unknown", AllowNull: false, Unique: false},                   // error: unsupported type
			"legacy_uuid":    {Type: "uuid", AllowNull: false, Unique: false},                      // error: uuid is no longer supported
			"missing_prefix": {Type: "uid", Prefix: "", AllowNull: false, Unique: true},            // error: uid requires a prefix when unique
			"bad_prefix":     {Type: "uid", Prefix: "User Prefix", AllowNull: false, Unique: true}, // error: prefix must be snake_case when unique
			"score":          {Type: "int32", AllowNull: false, Unique: false},
		},
		Operations: OperationConfig{
			Gets: []string{"id", "email", "non_existent"}, // non_existent: error; email: error due to not unique.
			Lists: []ListConfig{
				{
					Name:  "duplicate_name", // <-- We will duplicate this
					Where: "email = 'a@example.com'",
					Order: "created",
				},
				{
					Name:       "lst", // too short (must be > 4 chars)
					Where:      "email = 'a@example.com'",
					Order:      "email,created", // error: order must contain exactly one column
					Descending: false,
				},
				{
					Name:       "long_list",
					Where:      "",
					Order:      "unknown", // error: order column not defined in columns and not a default
					Descending: true,
				},
				{
					Name:       "invalid_mapping_list",
					Where:      "email = :badParam",
					Order:      "created",
					Descending: false,
					TypeMapping: map[string]string{
						"badParam": "ghost_column", // error: mapping to a column that doesn't exist
					},
				},
			},
			Deletes: []DeleteConfig{
				{
					Name: "del", // error: too short
				},
				{
					Name: "delete invalid mapping", // error: spaces, not snake_case
					TypeMapping: map[string]string{
						"badParam": "ghost_column", // error: ghost_column doesn't exist
					},
				},
			},
			Write:  true,
			Delete: true,
			ListsBulk: []ListBulkConfig{
				{
					Name:    "bad list name", // error: spaces, not snake case
					WhereIn: "ghost_column",  // error: column doesn't exist
				},
				{
					Name:    "lst", // error: too short
					WhereIn: "id",
				},
				{
					Name:    "list_by_public_ids",
					WhereIn: "id", // fails because id is inherently unique
				},
				{
					Name:    "list_by_unique_column",
					WhereIn: "id", // id is unique but we also define a custom unique below
				},
			},
			Plucks: []PluckConfig{
				{
					Name:   "duplicate_name", // <-- Collision!
					Column: "email",
					Where:  "status = :status",
				},
				{
					Name:   "plk", // error: too short
					Column: "email",
				},
				{
					Name:   "invalid pluck name", // error: not snake_case
					Column: "email",
				},
				{
					Name:   "missing_column_pluck",
					Column: "ghost_column", // error: column doesn't exist
				},
				{
					Name:   "bad_mapping_pluck",
					Column: "email",
					Where:  "email = :badParam",
					TypeMapping: map[string]string{
						"badParam": "ghost_column", // error: mapped column doesn't exist
					},
				},
			},
			UpsertsBulk: []UpsertBulkConfig{
				{
					Name:           "upsert invalid", // error: spaces, not snake_case
					ConflictTarget: "ghost_column",   // error: doesn't exist
					UpdateColumns:  []string{"id"},   // error: cannot update immutable column
					Increment:      true,
				},
			},
		},
		BufferedCounters: []BufferedCounterConfig{
			{
				Name:                 "inc",          // error: too short
				Column:               "email",        // error: must be numeric
				GroupBy:              "ghost_column", // error: doesn't exist
				FlushIntervalSeconds: 5,              // error: must be >= 10
			},
			{
				Name:                 "missing_upsert",
				Column:               "score",
				GroupBy:              "id",
				FlushIntervalSeconds: 300,
				// error: targeting 'score', but no upsertsBulk operation exists with Increment: true for 'score'
			},
		},
		CircuitBreaker: CircuitBreakerConfig{
			TimeoutSeconds:      0,
			ConsecutiveFailures: 0,
		},
		Caching: CachingConfig{
			Type:                    "redis",
			SingleExpirationSeconds: 1,
			ListExpirationSeconds:   120,     // <-- Set to > 60
			ListInvalidation:        "epoch", // error: must be 'expire' or 'none' when using BufferedCounters
			MaxItemsCount:           5,
		},
	}

	err := ValidateEntityConfig(invalidConfig)
	if err == nil {
		t.Error("Expected invalid config to produce errors, got nil")
	}
	errStr := err.Error()

	expectedErrors := []string{
		"entity name must be in snake_case",
		"version cannot be empty",
		"column name 'firstName' must be in snake_case",
		"column 'invalid' has unsupported type 'unknown'",
		"column 'legacy_uuid' has unsupported type 'uuid'",
		"column 'missing_prefix' of type 'uid' requires a non-empty 'prefix'",
		"prefix 'User Prefix' for column 'bad_prefix' must be in snake_case",
		"get operation refers to unknown column 'non_existent'",
		"get operation requires column 'email' to be unique",
		"list name 'lst' must be longer than 4 characters",
		"list 'lst' order must contain exactly one column",
		"order column 'unknown' in list 'long_list' is not defined and not a default column",
		"typeMapping column 'ghost_column' for param 'badParam' in list 'invalid_mapping_list' is not defined",
		"delete name 'del' must be longer than 4 characters",
		"delete name 'delete invalid mapping' must be in snake_case",
		"typeMapping column 'ghost_column' for param 'badParam' in delete 'delete invalid mapping' is not defined",
		"listsBulk name 'bad list name' must be in snake_case",
		"listsBulk 'bad list name' refers to unknown whereIn column 'ghost_column'",
		"listsBulk name 'lst' must be longer than 4 characters",
		"caching type 'redis' is not implemented yet",
		"singleExpirationSeconds must be greater than 1",
		"maxItemsCount must be greater than 10",
		"consecutiveFailures should be 1 or more",
		"timeoutSeconds should be 1 or more",
		"pluck name 'plk' must be longer than 4 characters",
		"pluck name 'invalid pluck name' must be in snake_case",
		"pluck 'missing_column_pluck' refers to unknown column 'ghost_column'",
		"typeMapping column 'ghost_column' for param 'badParam' in pluck 'bad_mapping_pluck' is not defined",
		"duplicate operation name 'duplicate_name' found in 'plucks' (would collide with 'DuplicateName' generated by 'lists')",
		"listsBulk 'list_by_public_ids' uses whereIn on 'id' which is unique. Use 'getsBulk' instead.",
		"listsBulk 'list_by_unique_column' uses whereIn on 'id' which is unique. Use 'getsBulk' instead.",
		"when listInvalidation is 'epoch', listExpirationSeconds must be 60 or less",
		"upsertsBulk name 'upsert invalid' must be in snake_case",
		"upsertsBulk 'upsert invalid' refers to unknown conflictTarget column 'ghost_column'",
		"upsertsBulk 'upsert invalid' cannot update immutable column 'id'",
		"bufferedCounter name 'inc' must be longer than 4 characters",
		"bufferedCounter 'inc' column 'email' must be a numeric type (int8/int32/int64) for atomic increments",
		"bufferedCounter 'inc' refers to unknown groupBy column 'ghost_column'",
		"bufferedCounter 'inc' flushIntervalSeconds must be at least 10 seconds to prevent Redis CPU spikes",
		"bufferedCounter 'inc' requires entity listInvalidation to be 'expire' or 'none', currently 'epoch'",
		"bufferedCounter 'missing_upsert' targeting column 'score' requires a corresponding 'upsertsBulk' operation with 'increment: true' and 'score' in 'updateColumns'",
	}

	for _, expectedError := range expectedErrors {
		if !strings.Contains(errStr, expectedError) {
			t.Errorf("Expected error message to contain: %q\nGot: %q", expectedError, errStr)
		}
	}
}
