package generator

import (
	"strings"
	"testing"
)

func TestFormatWhereClause(t *testing.T) {
	// Simulate a typical PurpleHoney entity schema with MySQL reserved keywords
	columns := map[string]Column{
		"order":      {Type: "int8"},
		"status":     {Type: "varchar"},
		"match_uid":  {Type: "uid"},
		"target_age": {Type: "int8"},
		"revoked":    {Type: "bool"},
		"expires_at": {Type: "datetime"},
	}

	tests := []struct {
		name     string
		where    string
		expected string
	}{
		{
			name:     "Reserved keyword column and matching named param",
			where:    "order = :order AND status = :status",
			expected: "`order` = ? AND `status` = ?",
		},
		{
			name:     "Built-in columns (id, created, updated, deleted_at, version)",
			where:    "created < :cutoff OR (revoked = true AND updated < :cutoff)",
			expected: "`created` < ? OR (`revoked` = true AND `updated` < ?)",
		},
		{
			name:     "String literals containing column names must not be backticked",
			where:    "status = 'order' AND match_uid = \"status\"",
			expected: "`status` = 'order' AND `match_uid` = \"status\"",
		},
		{
			name:     "Already backticked columns must not be double-backticked",
			where:    "`order` = :order AND `status` = 'active'",
			expected: "`order` = ? AND `status` = 'active'",
		},
		{
			name:     "Repeated named parameters",
			where:    "target_age = :minAge OR target_age > :minAge",
			expected: "`target_age` = ? OR `target_age` > ?",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actual := formatWhereClause(tc.where, columns)
			if actual != tc.expected {
				t.Errorf("formatWhereClause(%q)\ngot:  %q\nwant: %q", tc.where, actual, tc.expected)
			}
		})
	}
}

func TestListAndCountQueries(t *testing.T) {
	columns := map[string]Column{
		"order":  {Type: "int8"},
		"status": {Type: "varchar"},
	}

	listCfg := ListConfig{
		Name:       "list_by_order",
		Where:      "status = :status",
		Order:      "order",
		Descending: true,
	}

	t.Run("ListQuery_FirstPage_SoftDelete", func(t *testing.T) {
		q := listQuery(true, "quest_challenge", listCfg, columns, true)
		expected := "SELECT `id`, `version`, `order`, `status`, `created`, `updated`, `deleted_at` FROM `quest_challenges` WHERE (`status` = ?) AND `deleted_at` IS NULL ORDER BY `order` DESC, `id` DESC LIMIT ?"
		if q != expected {
			t.Errorf("got:  %s\nwant: %s", q, expected)
		}
	})

	t.Run("ListQuery_NextPage_Descending_SoftDelete", func(t *testing.T) {
		q := listQuery(false, "quest_challenge", listCfg, columns, true)
		expected := "SELECT `id`, `version`, `order`, `status`, `created`, `updated`, `deleted_at` FROM `quest_challenges` WHERE (`status` = ?) AND `deleted_at` IS NULL AND `id` < ? ORDER BY `order` DESC, `id` DESC LIMIT ?"
		if q != expected {
			t.Errorf("got:  %s\nwant: %s", q, expected)
		}
	})

	t.Run("CountQuery_SoftDelete", func(t *testing.T) {
		q := countQuery("quest_challenge", listCfg, columns, true)
		expected := "SELECT count(*) FROM `quest_challenges` WHERE (`status` = ?) AND `deleted_at` IS NULL"
		if q != expected {
			t.Errorf("got:  %s\nwant: %s", q, expected)
		}
	})
}

func TestDeleteQuery_DeterministicScrambling(t *testing.T) {
	columns := map[string]Column{
		"uid":         {Type: "uid", Prefix: "usr", Unique: true},
		"email":       {Type: "varchar", Unique: true},
		"invite_code": {Type: "varchar", Unique: true},
		"age":         {Type: "int8", Unique: false},
	}

	delCfg := DeleteConfig{
		Name:  "delete_older",
		Where: "age > :age",
	}

	// Run multiple times to prove Go map iteration order never scrambles the SQL string
	expectedSoft := "UPDATE `users` SET `deleted_at` = NOW(), `updated` = NOW(), `version` = `version` + 1, `email` = CONCAT(`email`, '-del-', UUID()), `invite_code` = CONCAT(`invite_code`, '-del-', UUID()), `uid` = CONCAT(`uid`, '-del-', UUID()) WHERE (`age` > ?) AND `deleted_at` IS NULL"
	for i := 0; i < 10; i++ {
		actual := deleteQuery("user", delCfg, columns, true, false)
		if actual != expectedSoft {
			t.Fatalf("iteration %d produced non-deterministic soft delete query:\ngot:  %s\nwant: %s", i, actual, expectedSoft)
		}
	}

	expectedHard := "DELETE FROM `users` WHERE `age` > ?"
	actualHard := deleteQuery("user", delCfg, columns, true, true)
	if actualHard != expectedHard {
		t.Errorf("got:  %s\nwant: %s", actualHard, expectedHard)
	}
}

func TestListSQLIndexes_PruningAndCoverage(t *testing.T) {
	columns := map[string]Column{
		"uid":        {Type: "uid", Prefix: "m", Unique: true},
		"couple_uid": {Type: "varchar", Unique: false},
		"status":     {Type: "varchar", Unique: false},
		"age":        {Type: "int8", Unique: false},
	}

	lists := []ListConfig{
		// Should produce idx_age_created
		{Name: "list_by_age", Where: "age = :age", Order: "created"},
	}
	listsBulk := []ListBulkConfig{
		// Should produce idx_couple_uid_status
		{Name: "list_by_couple_and_statuses", Where: "couple_uid = :couple_uid", WhereIn: "status"},
	}
	updatesBulk := []UpdateBulkConfig{
		// Single unique column 'uid' -> should be skipped because unique index already covers it
		{Name: "update_by_uids", Set: []string{"status"}, WhereIn: "uid"},
		// Non-unique column 'couple_uid' -> left-prefix of (couple_uid, status), so should be pruned!
		{Name: "update_by_couples", Set: []string{"status"}, WhereIn: "couple_uid"},
	}
	deletes := []DeleteConfig{
		// Single column 'age' -> left-prefix of (age, created), so should be pruned!
		{Name: "delete_by_age", Where: "age > :age"},
	}

	indexesSQL := listSQLIndexes("match", columns, lists, listsBulk, updatesBulk, nil, deletes)

	// 1. Composite indexes should exist
	if !strings.Contains(indexesSQL, "CREATE INDEX `idx_age_created` ON `matchs` (`age`, `created`);") {
		t.Errorf("Expected composite index idx_age_created, got:\n%s", indexesSQL)
	}
	if !strings.Contains(indexesSQL, "CREATE INDEX `idx_couple_uid_status` ON `matchs` (`couple_uid`, `status`);") {
		t.Errorf("Expected composite index idx_couple_uid_status, got:\n%s", indexesSQL)
	}

	// 2. Redundant left-prefix indexes must be pruned
	if strings.Contains(indexesSQL, "CREATE INDEX `idx_age` ") {
		t.Errorf("Expected redundant left-prefix idx_age to be pruned, got:\n%s", indexesSQL)
	}
	if strings.Contains(indexesSQL, "CREATE INDEX `idx_couple_uid` ") {
		t.Errorf("Expected redundant left-prefix idx_couple_uid to be pruned, got:\n%s", indexesSQL)
	}

	// 3. Unique column 'uid' must not get a duplicate non-unique index
	if strings.Contains(indexesSQL, "CREATE INDEX `idx_uid` ") {
		t.Errorf("Expected unique column 'uid' to not receive a redundant non-unique index, got:\n%s", indexesSQL)
	}
}
