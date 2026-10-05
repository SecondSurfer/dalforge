package generator

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"
)

//go:embed templates/*
//go:embed templates/**/*
var templateFS embed.FS

type Generator struct {
	dalContractTemplate *template.Template
	dalImplTemplate     *template.Template
	sqlTemplate         *template.Template
}

func NewGenerator() (*Generator, error) {
	funcMap := template.FuncMap{
		"toLower":                      strings.ToLower,
		"snakeCase":                    SnakeCaser,
		"pascalCase":                   PascalCaser,
		"camelCase":                    CamelCaser,
		"goField":                      PascalCaser,
		"goColumn":                     toColumnName,
		"toGoType":                     toGoType,
		"toSQLType":                    toSQLType,
		"dict":                         dict,
		"join":                         join,
		"keys":                         keys,
		"sub":                          sub,
		"add":                          add,
		"pluralize":                    pluralize,
		"querySelect":                  querySelect,
		"goFuncCallParameters":         goFuncCallParameters,
		"listQuery":                    listQuery,
		"countQuery":                   countQuery,
		"listQueryParams":              listQueryParams,
		"countQueryParams":             countQueryParams,
		"listFuncParams":               listFuncParams,
		"listFuncCallParams":           listFuncCallParams,
		"countFuncParams":              countFuncParams,
		"countFuncCallParams":          countFuncCallParams,
		"listCacheKey":                 listCacheKey,
		"countCacheKey":                countCacheKey,
		"listSQLIndexes":               listSQLIndexes,
		"checkColumnsChanged":          checkColumnsChanged,
		"invalidateUniqueColumnsCache": invalidateUniqueColumnsCache,
		"hasJSONColumn":                hasJSONColumn,
		"uniqueStringColumns":          uniqueStringColumns,
		"deleteQuery":                  deleteQuery,
		"deleteFuncParams":             deleteFuncParams,
		"deleteFuncCallParams":         deleteFuncCallParams,
		"deleteQueryParams":            deleteQueryParams,
		"bulkFuncParams":               bulkFuncParams,
		"bulkUpdateFuncParams":         bulkUpdateFuncParams,
		"bulkUpdateCallParams":         bulkUpdateCallParams,
		"listBulkFuncParams":           listBulkFuncParams,
		"listBulkInternalFuncParams":   listBulkInternalFuncParams,
		"listBulkInnerCallArgs":        listBulkInnerCallArgs,
		"listBulkQueryWhere":           listBulkQueryWhere,
		"extractParams":                extractParams,
		"trimPrefix":                   strings.TrimPrefix,
		"pluckFuncParams":              pluckFuncParams,
		"pluckFuncCallParams":          pluckFuncCallParams,
		"pluckQueryParams":             pluckQueryParams,
		"pluckCacheKey":                pluckCacheKey,
		"pluckQueryWhere":              pluckQueryWhere,
	}

	dalContractTmpl, err := template.New("contract").Funcs(funcMap).ParseFS(templateFS, "templates/dal/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("failed to parse DAL contract template: %w", err)
	}

	dalImplTmpl, err := template.New("impl").Funcs(funcMap).ParseFS(templateFS, "templates/dal/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("failed to parse DAL impl template: %w", err)
	}

	sqlTmpl, err := template.New("sql").Funcs(funcMap).ParseFS(templateFS, "templates/sql/*.tmpl")
	if err != nil {
		return nil, fmt.Errorf("failed to parse SQL template: %w", err)
	}

	return &Generator{
		dalContractTemplate: dalContractTmpl,
		dalImplTemplate:     dalImplTmpl,
		sqlTemplate:         sqlTmpl,
	}, nil
}

// GenerateDAL parses entity YAML and returns separate contract Go code, implementation Go code, or error.
func (g *Generator) GenerateDAL(yamlInput string) (contractCode string, implCode string, err error) {
	config, err := g.parseYAML(yamlInput)
	if err != nil {
		return "", "", err
	}

	config.TemplateVersion, err = getDirectoryHash(templateFS, ".")
	if err != nil {
		return "", "", err
	}

	// 1. Generate Contract ({entity}.gen.go)
	var contractBuf strings.Builder
	if err := g.dalContractTemplate.ExecuteTemplate(&contractBuf, "contract.tmpl", config); err != nil {
		return "", "", fmt.Errorf("DAL contract generation failed: %w", err)
	}

	// 2. Generate Implementation ({entity}_impl.gen.go)
	var implBuf strings.Builder
	if err := g.dalImplTemplate.ExecuteTemplate(&implBuf, "impl.tmpl", config); err != nil {
		return "", "", fmt.Errorf("DAL implementation generation failed: %w", err)
	}

	return contractBuf.String(), implBuf.String(), nil
}

func printScalabilityWarnings(config EntityConfig) {
	checkOrClause := func(opType, opName, where string) {
		if strings.Contains(strings.ToUpper(where), " OR ") {
			fmt.Printf("⚠️  SCALABILITY WARNING: %s operation '%s' in entity '%s' contains an 'OR' clause.\n", opType, opName, config.Name)
			fmt.Printf("   -> MySQL struggles to optimize 'OR' conditions and may resort to Full Table Scans.\n")
			fmt.Printf("   -> For large datasets (30M+ rows), consider splitting this into separate operations.\n\n")
		}
	}

	for _, l := range config.Operations.Lists {
		checkOrClause("List", l.Name, l.Where)
	}
	for _, lb := range config.Operations.ListsBulk {
		checkOrClause("ListBulk", lb.Name, lb.Where)
	}
	for _, p := range config.Operations.Plucks {
		checkOrClause("Pluck", p.Name, p.Where)
	}
	for _, d := range config.Operations.Deletes {
		checkOrClause("Delete", d.Name, d.Where)
	}
}

func (g *Generator) parseYAML(yamlInput string) (EntityConfig, error) {
	var config EntityConfig
	if err := yaml.Unmarshal([]byte(yamlInput), &config); err != nil {
		return EntityConfig{}, fmt.Errorf("YAML parsing failed: %w", err)
	}

	if config.Name == "" {
		return EntityConfig{}, fmt.Errorf("entity name is required")
	}

	for colName := range config.Columns {
		if colName == "id" || colName == "created" || colName == "updated" {
			return EntityConfig{}, fmt.Errorf("column name '%s' is reserved", colName)
		}
	}

	if config.Caching.ListInvalidation == "" {
		config.Caching.ListInvalidation = "flush"
	}

	err := ValidateEntityConfig(config)

	if err == nil {
		printScalabilityWarnings(config)
	}

	return config, err
}

func getDirectoryHash(efs embed.FS, inputDir string) (string, error) {
	hash := sha256.New()

	err := fs.WalkDir(efs, inputDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		file, err := efs.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		if _, err := io.Copy(hash, file); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return "", fmt.Errorf("failed to compute directory hash: %w", err)
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

type EntityConfig struct {
	TemplateVersion string               // This item is not loaded from yaml but is calculated at runtime
	Name            string               `yaml:"name"`
	Version         string               `yaml:"version"`
	Columns         map[string]Column    `yaml:"columns"`
	Operations      OperationConfig      `yaml:"operations"`
	Caching         CachingConfig        `yaml:"caching"`
	CircuitBreaker  CircuitBreakerConfig `yaml:"circuitbreaker"`
}

type CachingConfig struct {
	Type                    string `yaml:"type"`
	SingleExpirationSeconds int32  `yaml:"singleExpirationSeconds"`
	ListExpirationSeconds   int32  `yaml:"listExpirationSeconds"`
	ListInvalidation        string `yaml:"listInvalidation"`
	MaxItemsCount           int32  `yaml:"maxItemsCount"`
}

type CircuitBreakerConfig struct {
	TimeoutSeconds      int32 `yaml:"timeoutSeconds"`
	ConsecutiveFailures int32 `yaml:"consecutiveFailures"`
}

type Column struct {
	Type      string `yaml:"type"`
	AllowNull bool   `yaml:"allowNull"`
	Unique    bool   `yaml:"unique"`
	Prefix    string `yaml:"prefix"`
}

type UpdateBulkConfig struct {
	Name    string   `yaml:"name"`
	Set     []string `yaml:"set"`
	WhereIn string   `yaml:"whereIn"`
}

type PluckConfig struct {
	Name        string            `yaml:"name"`
	Column      string            `yaml:"column"`
	Where       string            `yaml:"where"`
	TypeMapping map[string]string `yaml:"typeMapping"`
}

type OperationConfig struct {
	Gets        []string           `yaml:"gets"`
	GetsBulk    []string           `yaml:"getsBulk"`
	Lists       []ListConfig       `yaml:"lists"`
	ListsBulk   []ListBulkConfig   `yaml:"listsBulk"`
	Deletes     []DeleteConfig     `yaml:"deletes"`
	UpdatesBulk []UpdateBulkConfig `yaml:"updatesBulk"`
	Plucks      []PluckConfig      `yaml:"plucks"`
	Write       bool               `yaml:"write"`
	Delete      bool               `yaml:"delete"`
	SoftDelete  bool               `yaml:"softDelete"`
}

type DeleteConfig struct {
	Name        string            `yaml:"name"`
	Where       string            `yaml:"where"`
	TypeMapping map[string]string `yaml:"typeMapping"`
}

type ListConfig struct {
	Name        string            `yaml:"name"`
	Where       string            `yaml:"where"`
	Order       string            `yaml:"order"`
	Descending  bool              `yaml:"descending"`
	TypeMapping map[string]string `yaml:"typeMapping"`
}

type ListBulkConfig struct {
	Name        string            `yaml:"name"`
	Where       string            `yaml:"where"`
	WhereIn     string            `yaml:"whereIn"`
	TypeMapping map[string]string `yaml:"typeMapping"`
}
