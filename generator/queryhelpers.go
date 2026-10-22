package generator

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

func querySelect(columns map[string]Column, softDelete bool) string {
	result := "`id`, `version`, "

	var names []string
	for name := range columns {
		names = append(names, name)
	}
	sort.Strings(names) // Sort alphabetically for deterministic SQL generation

	for _, name := range names {
		result = result + fmt.Sprintf("`%s`, ", SnakeCaser(name))
	}

	result += "`created`, `updated`"

	// Inject deleted_at if soft deletes are enabled
	if softDelete {
		result += ", `deleted_at`"
	}

	return result
}

// if given input: "deleted = 0 AND target = :target AND age = :age"
// gives output: ["target", "age"]
func extractParams(input string) []string {
	// Regular expression to match placeholders starting with ':'
	re := regexp.MustCompile(`:(\w+)`)

	// Find all matches
	matches := re.FindAllStringSubmatch(input, -1)

	// Extract parameter names
	var params []string
	for _, match := range matches {
		if len(match) > 1 {
			params = append(params, match[1])
		}
	}

	return params
}

// extractUniqueParams returns a deduplicated list of parameters while preserving order.
// This is used for generating Go function signatures and Cache keys.
func extractUniqueParams(input string) []string {
	params := extractParams(input)
	var unique []string
	seen := make(map[string]bool)

	for _, param := range params {
		if !seen[param] {
			seen[param] = true
			unique = append(unique, param)
		}
	}

	return unique
}

// replaceParams replaces all named parameters (e.g., :target) with "?" for SQL placeholders.
func replaceParams(query string) string {
	re := regexp.MustCompile(`:(\w+)`)
	return re.ReplaceAllString(query, "?")
}

// formatWhereClause replaces named parameters (:param -> ?) and wraps recognized
// table column names in backticks so MySQL reserved keywords (e.g., `order`, `status`, `match`)
// in custom YAML where clauses never cause syntax errors.
// It safely ignores single-quoted strings ('...'), double-quoted strings ("..."), and
// identifiers that are already wrapped in backticks (`...`).
func formatWhereClause(where string, columns map[string]Column) string {
	// 1. Replace :named_params with ? first so parameter names are never mistaken for columns.
	s := replaceParams(where)

	isColumn := func(word string) bool {
		if word == "id" || word == "version" || word == "created" || word == "updated" || word == "deleted_at" {
			return true
		}
		_, ok := columns[word]
		return ok
	}

	var out strings.Builder
	n := len(s)
	i := 0

	for i < n {
		ch := s[i]

		// Pass through already-backticked identifiers untouched: `...`
		if ch == '`' {
			out.WriteByte(ch)
			i++
			for i < n && s[i] != '`' {
				out.WriteByte(s[i])
				i++
			}
			if i < n {
				out.WriteByte(s[i])
				i++
			}
			continue
		}

		// Pass through single-quoted or double-quoted SQL string literals untouched
		if ch == '\'' || ch == '"' {
			quote := ch
			out.WriteByte(ch)
			i++
			for i < n && s[i] != quote {
				if s[i] == '\\' && i+1 < n {
					out.WriteByte(s[i])
					i++
				}
				out.WriteByte(s[i])
				i++
			}
			if i < n {
				out.WriteByte(s[i])
				i++
			}
			continue
		}

		// Check if start of an identifier word
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_' {
			start := i
			for i < n && ((s[i] >= 'a' && s[i] <= 'z') || (s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= '0' && s[i] <= '9') || s[i] == '_') {
				i++
			}
			word := s[start:i]
			if isColumn(word) {
				out.WriteString("`")
				out.WriteString(SnakeCaser(word))
				out.WriteString("`")
			} else {
				out.WriteString(word)
			}
			continue
		}

		out.WriteByte(ch)
		i++
	}

	return out.String()
}

/*
	Makes the where part of this query.

query = SELECT * FROM `posts` WHERE

	`deleted` = 0 and `age` = ?  # if where != nil; validate those columns are valid and indexed properly
	AND id < ?  # Pagination if descending is true and startId != 0
	AND id > ?  # Pagination if descending is false and startId != 0
*/
func listWhereQuery(isStartIdZero bool, list ListConfig, columns map[string]Column, softDelete bool) string {
	result := ""
	// if we have custom where clause
	if strings.TrimSpace(list.Where) != "" {
		result += "(" + formatWhereClause(list.Where, columns) + ")"
	}

	// Inject the soft delete scope
	if softDelete {
		if result != "" {
			result += " AND "
		}
		result += "`deleted_at` IS NULL"
	}

	if list.Descending && !isStartIdZero && strings.TrimSpace(list.Order) != "" {
		if result != "" {
			result += " AND "
		}
		result += "`id` < ?"
	} else if !list.Descending && !isStartIdZero {
		if result != "" {
			result += " AND "
		}
		result += "`id` > ?"
	}

	return result
}

func listQuery(isStartIdZero bool, entityName string, list ListConfig, columns map[string]Column, softDelete bool) string {
	result := ""
	where := listWhereQuery(isStartIdZero, list, columns, softDelete)

	result = fmt.Sprintf("SELECT %s FROM `%ss`", querySelect(columns, softDelete), SnakeCaser(entityName))
	if where != "" {
		result += fmt.Sprintf(" WHERE %s", where)
	}

	if strings.TrimSpace(list.Order) != "" {
		result += fmt.Sprintf(" ORDER BY `%s`", SnakeCaser(list.Order))
		if list.Descending {
			result += " DESC, `id` DESC"
		} else {
			result += ", `id`"
		}
	} else {
		result += " ORDER BY `id`"
	}

	result += " LIMIT ?"
	return result
}

func countQuery(entityName string, list ListConfig, columns map[string]Column, softDelete bool) string {
	result := ""
	where := listWhereQuery(true, list, columns, softDelete)

	result = fmt.Sprintf("SELECT count(*) FROM `%ss`", SnakeCaser(entityName))
	if where != "" {
		result += fmt.Sprintf(" WHERE %s", where)
	}

	return result
}

// Input: "email", { allowNulls: true, forceLowercase: true }
// Output: strings.ToLower(email.String)
//
// Input: "email", { allowNulls: true, forceLowercase: false }
// Output: email
func columnParameter(structNamePrefix string, colName string, col Column) string {
	if structNamePrefix != "" {
		structNamePrefix = structNamePrefix + "."
	}

	return fmt.Sprintf("%s%s", structNamePrefix, PascalCaser(colName))
}

// Outputs string that is used for this:
// rows, err := db.QueryContext(ctx, query, {{queryParams .Columns}})
// should created something like this as output:
// rows, err := db.QueryContext(ctx, query, structNamePrefix.email, structNamePrefix.age, structNamePrefix.pageSize)
func goFuncCallParameters(structNamePrefix string, columns map[string]Column) string {
	var result []string
	keys := make([]string, 0, len(columns))

	// Extract keys
	for colName := range columns {
		keys = append(keys, colName)
	}

	// Sort keys alphabetically
	sort.Strings(keys)

	// Process in sorted order
	for _, colName := range keys {
		result = append(result, columnParameter(structNamePrefix, colName, columns[colName]))
	}

	return strings.Join(result, ", ")
}

// Outputs string that is used for this:
// rows, err := db.QueryContext(ctx, query, {{listQueryParams .List}})
// should created something like this as output:
// rows, err := db.QueryContext(ctx, query, startID, age, pageSize)
func listQueryParams(isStartIdZero bool, list ListConfig, columns map[string]Column) string {
	result := ""
	params := extractParams(list.Where)
	for _, param := range params {
		result += fmt.Sprintf("%s, ", CamelCaser(param))
	}

	if !isStartIdZero {
		result += "startID, pageSize"
	} else {
		result += "pageSize"
	}

	return result
}

// Outputs string that is used for this:
// d.Somefunction(ctx, {{listFuncCallParams .List}})
// should created something like this as output:
// d.Somefunction(ctx, query, startID, age, pageSize)
func listFuncCallParams(isStartIdZero bool, list ListConfig, columns map[string]Column) string {
	result := ""
	params := extractUniqueParams(list.Where)
	for _, param := range params {

		result += fmt.Sprintf("%s, ", CamelCaser(param))
	}

	if !isStartIdZero {
		result += "startID, pageSize"
	} else {
		result += "pageSize"
	}

	return result
}

// Outputs string that is used for this:
// rows, err := db.QueryContext(ctx, query, {{countQueryParams .List}})
// should created something like this as output:
// rows, err := db.QueryContext(ctx, query, age)
func countQueryParams(list ListConfig, columns map[string]Column) string {
	result := ""
	params := extractParams(list.Where)
	for _, param := range params {
		result += fmt.Sprintf("%s, ", CamelCaser(param))
	}

	// Clean up the trailing comma and space
	return strings.TrimSuffix(result, ", ")
}

// Outputs string that is used for this:
// d.FuncCall(ctx, {{countFuncCallParams .List}})
// should created something like this as output:
// d.FuncCall(ctx, age, userID)
func countFuncCallParams(list ListConfig, columns map[string]Column) string {
	result := ""
	params := extractUniqueParams(list.Where)
	for _, param := range params {
		result += fmt.Sprintf("%s, ", CamelCaser(param))
	}

	// Clean up the trailing comma and space
	return strings.TrimSuffix(result, ", ")
}

// for input:
// func (d *UserDAL) listByAge(ctx context.Context, {{listFuncParams .List .Root.Columns}}) ([]*User, error) {
// outputs:
// func (d *UserDAL) listByAge(ctx context.Context, age int, startID int64, pageSize int) ([]*User, error) {
func listFuncParams(list ListConfig, columns map[string]Column) (string, error) {
	result := ""
	params := extractUniqueParams(list.Where) // Deduplicated!
	for _, param := range params {
		colName := param
		if mappedCol, ok := list.TypeMapping[param]; ok {
			colName = mappedCol
		}

		var goType string

		// Handle built-in default columns that aren't explicitly in the columns map
		if colName == "id" {
			goType = "int64"
		} else if colName == "created" || colName == "updated" {
			goType = "time.Time"
		} else if col, ok := columns[colName]; ok {
			goType = toGoType(col.Type, col.AllowNull)
		} else {
			return "", fmt.Errorf("dal yaml definition error. missing column %s, which is used in where under list %s", colName, list.Name)
		}

		result += fmt.Sprintf("%s %s, ", CamelCaser(param), goType)
	}

	result += "startID int64, pageSize int"
	return result, nil
}

// for input:
// func (d *UserDAL) countListByAge(ctx context.Context, {{listFuncParams .List .Root.Columns}}) (int64, error) {
// outputs:
// func (d *UserDAL) countListByAge(ctx context.Context, age int) (int64, error) {
func countFuncParams(list ListConfig, columns map[string]Column) (string, error) {
	result := ""
	params := extractUniqueParams(list.Where) // Deduplicated!
	for _, param := range params {
		colName := param
		if mappedCol, ok := list.TypeMapping[param]; ok {
			colName = mappedCol
		}

		var goType string

		// Handle built-in default columns that aren't explicitly in the columns map
		if colName == "id" {
			goType = "int64"
		} else if colName == "created" || colName == "updated" {
			goType = "time.Time"
		} else if col, ok := columns[colName]; ok {
			goType = toGoType(col.Type, col.AllowNull)
		} else {
			return "", fmt.Errorf("dal yaml definition error. missing column %s, which is used in where under count list %s", colName, list.Name)
		}

		result += fmt.Sprintf("%s %s, ", CamelCaser(param), goType)
	}

	return strings.TrimSuffix(result, ", "), nil
}

// Create cache key similar to this:
// fmt.Sprintf("{{$entityTableName}}_{{.List.Name | snakeCase}}:%v:%d:%d", age, startID, pageSize)
func listCacheKey(entityName string, list ListConfig, columns map[string]Column, caching CachingConfig) string {
	key := fmt.Sprintf("%s_%s", SnakeCaser(entityName), SnakeCaser(list.Name))
	params := extractUniqueParams(list.Where)

	if caching.ListInvalidation == "epoch" {
		key += ":epoch_%d"
	}
	for range params {
		key += ":%v"
	}
	key += ":%d:%d"

	paramStr := ""
	if caching.ListInvalidation == "epoch" {
		paramStr += "d.getEpoch(), "
	}

	for _, param := range params {
		colName := param
		if mappedCol, ok := list.TypeMapping[param]; ok {
			colName = mappedCol
		}
		col := columns[colName]
		goName := CamelCaser(param)
		if col.AllowNull {
			paramStr += fmt.Sprintf(`func() interface{} { if %s == nil { return "<<null>>" }; return *%s }(), `, goName, goName)
		} else {
			paramStr += fmt.Sprintf("%s, ", goName)
		}
	}
	paramStr += "startID, pageSize"
	return fmt.Sprintf(`fmt.Sprintf("%s", %s)`, key, paramStr)
}

// Create cache key similar to this:
// fmt.Sprintf("{{$entityTableName}}_{{.List.Name | snakeCase}}:%v", age)
func countCacheKey(entityName string, list ListConfig, columns map[string]Column, caching CachingConfig) string {
	key := fmt.Sprintf("%s_count_%s", SnakeCaser(entityName), SnakeCaser(list.Name))
	params := extractUniqueParams(list.Where)

	if caching.ListInvalidation == "epoch" {
		key += ":epoch_%d"
	}
	for range params {
		key += ":%v"
	}

	paramStr := ""
	if caching.ListInvalidation == "epoch" {
		paramStr += "d.getEpoch(), "
	}

	for _, param := range params {
		colName := param
		if mappedCol, ok := list.TypeMapping[param]; ok {
			colName = mappedCol
		}
		col := columns[colName]
		goName := CamelCaser(param)
		if col.AllowNull {
			paramStr += fmt.Sprintf(`func() interface{} { if %s == nil { return "<<null>>" }; return *%s }(), `, goName, goName)
		} else {
			paramStr += fmt.Sprintf("%s, ", goName)
		}
	}
	paramStr = strings.TrimSuffix(paramStr, ", ")

	if paramStr == "" {
		return fmt.Sprintf(`fmt.Sprintf("%s")`, key)
	}
	return fmt.Sprintf(`fmt.Sprintf("%s", %s)`, key, paramStr)
}

// deleteQuery generates the raw SQL for a custom bulk delete operation.
// It handles both soft deletes (via UPDATE) and hard deletes (via DELETE FROM).
func deleteQuery(entityName string, del DeleteConfig, columns map[string]Column, softDelete bool, isHardDelete bool) string {
	where := ""
	if strings.TrimSpace(del.Where) != "" {
		where = formatWhereClause(del.Where, columns)
	}

	// Soft Delete Logic
	if softDelete && !isHardDelete {
		result := fmt.Sprintf("UPDATE `%ss` SET `deleted_at` = NOW(), `updated` = NOW(), `version` = `version` + 1", SnakeCaser(entityName))

		// Scramble unique string columns to free up the unique constraint
		uniqueCols := uniqueStringColumns(columns)
		sort.Strings(uniqueCols) // Sort for deterministic SQL output
		for _, col := range uniqueCols {
			snakeCol := SnakeCaser(col)
			result += fmt.Sprintf(", `%s` = CONCAT(`%s`, '-del-', UUID())", snakeCol, snakeCol)
		}

		if where != "" {
			result += fmt.Sprintf(" WHERE (%s) AND `deleted_at` IS NULL", where)
		} else {
			result += " WHERE `deleted_at` IS NULL"
		}

		return result
	}

	// Hard Delete Logic
	result := fmt.Sprintf("DELETE FROM `%ss`", SnakeCaser(entityName))
	if where != "" {
		result += fmt.Sprintf(" WHERE %s", where)
	}

	return result
}

// deleteFuncParams generates the typed arguments for the Go function signature.
func deleteFuncParams(del DeleteConfig, columns map[string]Column) (string, error) {
	result := ""
	params := extractUniqueParams(del.Where)

	for _, param := range params {
		colName := param
		if mappedCol, ok := del.TypeMapping[param]; ok {
			colName = mappedCol
		}

		var goType string
		if colName == "id" {
			goType = "int64"
		} else if colName == "created" || colName == "updated" {
			goType = "time.Time"
		} else if col, ok := columns[colName]; ok {
			goType = toGoType(col.Type, col.AllowNull)
		} else {
			return "", fmt.Errorf("dal yaml definition error: missing column %s", colName)
		}

		result += fmt.Sprintf("%s %s, ", CamelCaser(param), goType)
	}

	// Always append the limit parameter
	return result + "limit int", nil
}

// deleteFuncCallParams generates the comma-separated variables passed as function call
func deleteFuncCallParams(del DeleteConfig) string {
	result := ""
	params := extractUniqueParams(del.Where)
	for _, param := range params {
		result += fmt.Sprintf("%s, ", CamelCaser(param))
	}

	// Always append the limit parameter
	return result + "limit"
}

// deleteQueryParams generates the comma-separated variables passed to db.ExecContext.
// Example Output: cutoff, status
func deleteQueryParams(del DeleteConfig) string {
	result := ""
	params := extractParams(del.Where)
	for _, param := range params {
		result += fmt.Sprintf("%s, ", CamelCaser(param))
	}

	return strings.TrimSuffix(result, ", ")
}

// pluralize is a simple helper to make parameter names plural for slices.
// e.g., "uid" -> "uids", "id" -> "ids", "status" -> "statuses"
func pluralize(s string) string {
	if strings.HasSuffix(s, "s") {
		return s + "es"
	}
	return s + "s"
}

// bulkFuncParams generates the parameter signature for a bulk get operation.
// Example Output: "uids []string" or "ids []int64"
func bulkFuncParams(colName string, columns map[string]Column) (string, error) {
	var goType string

	if colName == "id" {
		goType = "int64"
	} else if col, ok := columns[colName]; ok {
		// We force allowNull to false here because IN clauses typically use non-pointer slices
		goType = strings.TrimPrefix(toGoType(col.Type, false), "*")
	} else {
		return "", fmt.Errorf("column %s not found for bulk operation", colName)
	}

	paramName := CamelCaser(pluralize(colName))
	return fmt.Sprintf("%s []%s", paramName, goType), nil
}

// bulkUpdateFuncParams generates the typed arguments for a bulk update function.
// Example Output: "status string, points int32, uids []string"
func bulkUpdateFuncParams(upd UpdateBulkConfig, columns map[string]Column) (string, error) {
	result := ""

	// 1. Add the SET parameters
	for _, setCol := range upd.Set {
		col, ok := columns[setCol]
		if !ok {
			return "", fmt.Errorf("column %s not found for bulk update", setCol)
		}
		goType := toGoType(col.Type, col.AllowNull)
		result += fmt.Sprintf("%s %s, ", CamelCaser(setCol), goType)
	}

	// 2. Add the IN clause slice parameter
	var whereType string
	if upd.WhereIn == "id" {
		whereType = "int64"
	} else if col, ok := columns[upd.WhereIn]; ok {
		whereType = strings.TrimPrefix(toGoType(col.Type, false), "*")
	} else {
		return "", fmt.Errorf("column %s not found for bulk update whereIn", upd.WhereIn)
	}

	whereParam := CamelCaser(pluralize(upd.WhereIn))
	result += fmt.Sprintf("%s []%s", whereParam, whereType)

	return result, nil
}

// bulkUpdateCallParams generates the comma-separated variable names for calling the inner DB function.
// Example Output: "status, points, uids"
func bulkUpdateCallParams(upd UpdateBulkConfig) string {
	result := ""
	for _, setCol := range upd.Set {
		result += fmt.Sprintf("%s, ", CamelCaser(setCol))
	}
	whereParam := CamelCaser(pluralize(upd.WhereIn))
	result += whereParam

	return result
}

// listBulkFuncParams generates the public function signature parameters.
// Example: "user int64, storyUids []string"
func listBulkFuncParams(list ListBulkConfig, columns map[string]Column) (string, error) {
	result := ""

	// 1. Process regular 'Where' parameters
	if list.Where != "" {
		params := extractUniqueParams(list.Where)
		for _, param := range params {
			colName := param
			if mappedCol, ok := list.TypeMapping[param]; ok {
				colName = mappedCol
			}

			var goType string
			if colName == "id" {
				goType = "int64"
			} else if colName == "created" || colName == "updated" {
				goType = "time.Time"
			} else if col, ok := columns[colName]; ok {
				goType = toGoType(col.Type, col.AllowNull)
			} else {
				return "", fmt.Errorf("dal yaml definition error. missing column %s used in listsBulk %s", colName, list.Name)
			}

			result += fmt.Sprintf("%s %s, ", CamelCaser(param), goType)
		}
	}

	// 2. Process 'WhereIn' parameter (slice)
	var inGoType string
	if list.WhereIn == "id" {
		inGoType = "int64"
	} else if col, ok := columns[list.WhereIn]; ok {
		inGoType = strings.TrimPrefix(toGoType(col.Type, false), "*")
	} else {
		return "", fmt.Errorf("column %s not found for listsBulk whereIn", list.WhereIn)
	}

	inParamName := CamelCaser(pluralize(list.WhereIn))
	result += fmt.Sprintf("%s []%s", inParamName, inGoType)

	return result, nil
}

// listBulkInternalFuncParams generates the private function signature parameters.
// Example: "user int64, chunkArgs []interface{}"
func listBulkInternalFuncParams(list ListBulkConfig, columns map[string]Column) (string, error) {
	result := ""
	if list.Where != "" {
		params := extractUniqueParams(list.Where)
		for _, param := range params {
			colName := param
			if mappedCol, ok := list.TypeMapping[param]; ok {
				colName = mappedCol
			}

			var goType string
			if colName == "id" {
				goType = "int64"
			} else if colName == "created" || colName == "updated" {
				goType = "time.Time"
			} else if col, ok := columns[colName]; ok {
				goType = toGoType(col.Type, col.AllowNull)
			}
			result += fmt.Sprintf("%s %s, ", CamelCaser(param), goType)
		}
	}
	result += "chunkArgs []interface{}"
	return result, nil
}

// listBulkInnerCallArgs generates the variables to pass to the inner DB function.
// Example: "user, chunk"
func listBulkInnerCallArgs(list ListBulkConfig) string {
	result := ""
	if list.Where != "" {
		params := extractUniqueParams(list.Where)
		for _, param := range params {
			result += fmt.Sprintf("%s, ", CamelCaser(param))
		}
	}
	// BUG FIX: We must pass the converted `args` slice, NOT the strongly-typed `chunk` slice.
	result += "args"
	return result
}

// listBulkQueryWhere constructs the raw SQL WHERE clause.
// Example: "(user = ?) AND story_uid IN (%s) AND deleted_at IS NULL"
func listBulkQueryWhere(list ListBulkConfig, columns map[string]Column, softDelete bool) string {
	result := ""
	if strings.TrimSpace(list.Where) != "" {
		result += "(" + formatWhereClause(list.Where, columns) + ") AND "
	}

	result += fmt.Sprintf("`%s` IN (%%s)", SnakeCaser(list.WhereIn))

	if softDelete {
		result += " AND `deleted_at` IS NULL"
	}
	return result
}

func pluckFuncParams(pluck PluckConfig, columns map[string]Column) (string, error) {
	result := ""
	params := extractUniqueParams(pluck.Where)
	for _, param := range params {
		colName := param
		if mappedCol, ok := pluck.TypeMapping[param]; ok {
			colName = mappedCol
		}
		var goType string
		if colName == "id" {
			goType = "int64"
		} else if colName == "created" || colName == "updated" {
			goType = "time.Time"
		} else if col, ok := columns[colName]; ok {
			goType = toGoType(col.Type, col.AllowNull)
		} else {
			return "", fmt.Errorf("missing column %s in pluck %s", colName, pluck.Name)
		}
		result += fmt.Sprintf("%s %s, ", CamelCaser(param), goType)
	}
	return strings.TrimSuffix(result, ", "), nil
}

func pluckFuncCallParams(pluck PluckConfig) string {
	result := ""
	params := extractUniqueParams(pluck.Where)
	for _, param := range params {
		result += fmt.Sprintf("%s, ", CamelCaser(param))
	}
	return strings.TrimSuffix(result, ", ")
}

func pluckQueryParams(pluck PluckConfig) string {
	result := ""
	params := extractParams(pluck.Where)
	for _, param := range params {
		result += fmt.Sprintf("%s, ", CamelCaser(param))
	}
	return strings.TrimSuffix(result, ", ")
}

func pluckCacheKey(entityName string, pluck PluckConfig, columns map[string]Column, caching CachingConfig) string {
	key := fmt.Sprintf("%s_pluck_%s", SnakeCaser(entityName), SnakeCaser(pluck.Name))
	params := extractUniqueParams(pluck.Where)

	if caching.ListInvalidation == "epoch" {
		key += ":epoch_%d"
	}
	for range params {
		key += ":%v"
	}

	paramStr := ""
	if caching.ListInvalidation == "epoch" {
		paramStr += "d.getEpoch(), "
	}

	for _, param := range params {
		colName := param
		if mappedCol, ok := pluck.TypeMapping[param]; ok {
			colName = mappedCol
		}
		col := columns[colName]
		goName := CamelCaser(param)
		if col.AllowNull {
			paramStr += fmt.Sprintf(`func() interface{} { if %s == nil { return "<<null>>" }; return *%s }(), `, goName, goName)
		} else {
			paramStr += fmt.Sprintf("%s, ", goName)
		}
	}
	paramStr = strings.TrimSuffix(paramStr, ", ")

	if paramStr == "" {
		return fmt.Sprintf(`fmt.Sprintf("%s")`, key)
	}
	return fmt.Sprintf(`fmt.Sprintf("%s", %s)`, key, paramStr)
}

func pluckQueryWhere(pluck PluckConfig, columns map[string]Column, softDelete bool) string {
	result := ""
	if strings.TrimSpace(pluck.Where) != "" {
		result += "(" + formatWhereClause(pluck.Where, columns) + ")"
	}
	if softDelete {
		if result != "" {
			result += " AND "
		}
		result += "`deleted_at` IS NULL"
	}
	return result
}
