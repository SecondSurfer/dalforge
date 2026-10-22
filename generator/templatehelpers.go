package generator

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// Add these helper functions
var (
	titleCaser = cases.Title(language.English, cases.NoLower)
	lowerCaser = cases.Lower(language.English)
)

// PascalCaser converts a string to PascalCase
func PascalCaser(s string) string {
	parts := splitIntoParts(s)
	for i, p := range parts {
		parts[i] = titleCaser.String(lowerCaser.String(p))
	}
	return strings.Join(parts, "")
}

// CamelCaser converts a string to camelCase
func CamelCaser(s string) string {
	parts := splitIntoParts(s)
	for i, p := range parts {
		if i == 0 {
			// First word is lowercase
			parts[i] = lowerCaser.String(p)
		} else {
			// Subsequent words are title-cased
			parts[i] = titleCaser.String(lowerCaser.String(p))
		}
	}
	return strings.Join(parts, "")
}

// SnakeCaser converts a string to snake_case
func SnakeCaser(s string) string {
	var result []rune
	for i, r := range s {
		if unicode.IsUpper(r) {
			// Add underscore before uppercase letters (except the first character)
			if i > 0 {
				result = append(result, '_')
			}
			result = append(result, unicode.ToLower(r))
		} else {
			result = append(result, r)
		}
	}
	return string(result)
}

// Helper function to split a string into parts
func splitIntoParts(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == '_' || r == ' ' || r == '-'
	})
}

func toColumnName(s string) string {
	return lowerCaser.String(s)
}

func toGoType(yamlType string, allowNull bool) string {
	switch yamlType {
	case "int8":
		if allowNull {
			return "*int8" // int8 maps to NullInt16 in Go
		}
		return "int8"
	case "int32":
		if allowNull {
			return "*int32"
		}
		return "int32"
	case "int64":
		if allowNull {
			return "*int64"
		}
		return "int64"
	case "float":
		if allowNull {
			return "*float"
		}
		return "float64"
	case "uid":
		if allowNull {
			return "*string"
		}
		return "string"
	case "varchar":
		if allowNull {
			return "*string"
		}
		return "string"
	case "text":
		if allowNull {
			return "*string"
		}
		return "string"
	case "bool":
		if allowNull {
			return "*bool"
		}
		return "bool"
	case "date", "time", "datetime":
		if allowNull {
			return "*time.Time"
		}
		return "time.Time"
	case "json":
		if allowNull {
			return "*json.RawMessage"
		}
		return "json.RawMessage"
	default:
		return "interface{}" // Fallback for unknown types
	}
}

func toSQLType(yamlType string) string {
	switch yamlType {
	case "int8":
		return "TINYINT"
	case "int32":
		return "INT"
	case "int64":
		return "BIGINT"
	case "float":
		return "DOUBLE"
	case "varchar":
		return "VARCHAR(255)" // Default length for strings
	case "text":
		return "TEXT" // Default length for strings
	case "bool":
		return "BOOLEAN"
	case "date":
		return "DATE"
	case "time":
		return "TIME"
	case "datetime":
		return "DATETIME"
	case "uid":
		return "VARCHAR(255)"
	case "json": // Add JSON case
		return "JSON"
	default:
		return "TEXT" // Fallback for unknown types
	}
}

func dict(values ...interface{}) map[string]interface{} {
	if len(values)%2 != 0 {
		panic("dict must have even number of arguments")
	}
	m := make(map[string]interface{})
	for i := 0; i < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			panic("dict keys must be strings")
		}
		m[key] = values[i+1]
	}
	return m
}

func join(sep string, items ...string) string {
	return strings.Join(items, sep)
}

func keys(m interface{}) []string {
	var result []string

	switch v := m.(type) {
	case map[string]Column: // ✅ Handle map[string]Column
		for key := range v {
			result = append(result, key)
		}
	case map[string]interface{}:
		for key := range v {
			result = append(result, key)
		}
	default:
		panic(fmt.Sprintf("keys function expects a map[string]Column or map[string]interface{}, got %T", m))
	}

	return result
}

func sub(a, b int) int {
	return a - b
}

func add(a, b int) int {
	return a + b
}

// extractIndexColumns smartly extracts recognized column names from SQL strings
func extractIndexColumns(where string, order string, columns map[string]Column) []string {
	var cols []string
	seen := make(map[string]bool)

	//ARCHITECTURE: Strip named parameters (e.g., :cutoff) before scanning words
	// so parameter names are never mistaken for table columns.
	cleanWhere := replaceParams(where)

	// Regex to match whole words (potential column names)
	re := regexp.MustCompile(`\b[a-zA-Z_][a-zA-Z0-9_]*\b`)

	// 1. Extract from WHERE clause
	whereWords := re.FindAllString(cleanWhere, -1)
	for _, w := range whereWords {
		isBuiltIn := w == "id" || w == "created" || w == "updated" || w == "deleted_at"
		_, isCustom := columns[w]

		if (isCustom || isBuiltIn) && !seen[w] {
			seen[w] = true
			cols = append(cols, w)
		}
	}

	// 2. Extract from ORDER clause (appended to the end of the index)
	orderWords := re.FindAllString(order, -1)
	for _, w := range orderWords {
		isBuiltIn := w == "id" || w == "created" || w == "updated" || w == "deleted_at"
		_, isCustom := columns[w]

		if (isCustom || isBuiltIn) && !seen[w] {
			seen[w] = true
			cols = append(cols, w)
		}
	}

	return cols
}

func listSQLIndexes(
	tableName string,
	columns map[string]Column,
	lists []ListConfig,
	listsBulk []ListBulkConfig,
	updatesBulk []UpdateBulkConfig,
	plucks []PluckConfig,
	deletes []DeleteConfig,
) string {
	var candidateCols [][]string

	addCandidate := func(cols []string) {
		if len(cols) == 0 {
			return
		}
		// ARCHITECTURE: Skip single-column indexes on 'id' (covered by PRIMARY KEY)
		// or columns marked 'unique: true' (already covered by CREATE UNIQUE INDEX).
		if len(cols) == 1 {
			if cols[0] == "id" {
				return
			}
			if col, ok := columns[cols[0]]; ok && col.Unique {
				return
			}
		}
		candidateCols = append(candidateCols, cols)
	}

	// 1. Process Standard Lists
	for _, list := range lists {
		addCandidate(extractIndexColumns(list.Where, list.Order, columns))
	}

	// 2. Process Bulk Lists
	for _, listBulk := range listsBulk {
		cols := extractIndexColumns(listBulk.Where, "", columns)
		if listBulk.WhereIn != "" && listBulk.WhereIn != "id" && !slices.Contains(cols, listBulk.WhereIn) {
			cols = append(cols, listBulk.WhereIn)
		}
		addCandidate(cols)
	}

	// 3. Process Bulk Updates (Ensure WHERE IN column is indexed to avoid full-table locking)
	for _, updBulk := range updatesBulk {
		if updBulk.WhereIn != "" && updBulk.WhereIn != "id" {
			addCandidate([]string{updBulk.WhereIn})
		}
	}

	// 4. Process Plucks (Covering Indexes)
	for _, pluck := range plucks {
		cols := extractIndexColumns(pluck.Where, "", columns)
		if pluck.Column != "id" && !slices.Contains(cols, pluck.Column) {
			cols = append(cols, pluck.Column)
		}
		addCandidate(cols)
	}

	// 5. Process Deletes
	for _, del := range deletes {
		addCandidate(extractIndexColumns(del.Where, "", columns))
	}

	// Prune duplicates and redundant left-prefix indexes (e.g., drop [age] if [age, created] exists)
	indexMap := make(map[string]string)
	for i, colsA := range candidateCols {
		isRedundantPrefix := false
		for j, colsB := range candidateCols {
			if i == j || len(colsA) > len(colsB) {
				continue
			}
			// If identical length and same elements, only keep the first occurrence
			if len(colsA) == len(colsB) && slices.Equal(colsA, colsB) && i > j {
				isRedundantPrefix = true
				break
			}
			// If colsA is a strict left-prefix of colsB (e.g., [age] is covered by [age, created])
			if len(colsA) < len(colsB) && slices.Equal(colsA, colsB[:len(colsA)]) {
				isRedundantPrefix = true
				break
			}
		}

		if isRedundantPrefix {
			continue
		}

		colSignature := strings.Join(colsA, "_")
		idxName := fmt.Sprintf("idx_%s", colSignature)
		if len(idxName) > 64 {
			idxName = idxName[:64]
		}

		quotedCols := make([]string, len(colsA))
		for k, col := range colsA {
			quotedCols[k] = fmt.Sprintf("`%s`", SnakeCaser(col))
		}
		colStr := strings.Join(quotedCols, ", ")
		indexMap[colSignature] = fmt.Sprintf("CREATE INDEX `%s` ON `%ss` (%s);\n", idxName, SnakeCaser(tableName), colStr)
	}

	// Sort the map keys to ensure deterministic SQL generation output
	var keys []string
	for k := range indexMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var builder strings.Builder
	for _, k := range keys {
		builder.WriteString(indexMap[k])
	}

	return builder.String()
}

/*
	 Would output something like this for any column that has get operation:
		var oldEmail string
		if existing.Email != entity.Email {
			oldEmail = existing.Email
		}
*/
func checkColumnsChanged(config EntityConfig) string {
	result := ""
	for _, colName := range config.Operations.Gets {
		if colName == "id" {
			continue
		}
		col := config.Columns[colName]
		result += fmt.Sprintf(`
	var old%s %s
	if existing.%s != entity.%s {
		old%s = existing.%s
	}
		`, PascalCaser(colName), toGoType(col.Type, col.AllowNull), PascalCaser(colName), PascalCaser(colName),
			PascalCaser(colName), PascalCaser(colName))
	}

	return result
}

func invalidateUniqueColumnsCache(config EntityConfig) string {
	result := ""
	entityTableName := SnakeCaser(config.Name)

	for _, colName := range config.Operations.Gets {
		if colName == "id" {
			continue
		}
		result += fmt.Sprintf(`
	if old%s != "" {
		oldCacheKey := fmt.Sprintf("%s_%s:%%s", old%s)
		d.cache.Delete(oldCacheKey)
		d.cacheProvider.InvalidateCache("%s", oldCacheKey)
	}`, PascalCaser(colName), entityTableName, SnakeCaser(colName), PascalCaser(colName), entityTableName)
	}

	return result
}

// hasJSONColumn checks if any column in the entity is of type "json".
// This is used to conditionally import the "encoding/json" package in generated code.
func hasJSONColumn(columns map[string]Column) bool {
	for _, col := range columns {
		if col.Type == "json" {
			return true
		}
	}
	return false
}

func uniqueStringColumns(columns map[string]Column) []string {
	var uniqueCols []string
	for name, col := range columns {
		// Only scramble string-based unique columns
		if col.Unique && (col.Type == "varchar" || col.Type == "uid" || col.Type == "text") {
			uniqueCols = append(uniqueCols, name)
		}
	}
	sort.Strings(uniqueCols)
	return uniqueCols
}
