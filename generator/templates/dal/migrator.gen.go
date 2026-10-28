package dal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// Domain errors for the schema and data migration tool.
var (
	// SRE FIX: Include /v3 in module path for Go Semantic Import Versioning
	ErrMysqldefNotFound = errors.New("mysqldef binary not found in PATH (run: go install github.com/sqldef/sqldef/v3/cmd/mysqldef@v3.11.28)")
	ErrNoSQLFiles       = errors.New("no embedded .sql files found in schema filesystem")
)

// schemaMigrationsDDL defines the ledger table used by Phase 2 imperative SQL data migrations.
// Architectural Why: Because DALForge natively manages both declarative schema (Phase 1) and
// versioned SQL data migrations (Phase 2), embedding this table definition into Phase 1's
// desiredSQL stream ensures mysqldef never proposes dropping the ledger during --enable-drop runs.
const schemaMigrationsDDL = `-- Source: _dalforge_schema_migrations.sql
CREATE TABLE schema_migrations (
    version VARCHAR(255) NOT NULL PRIMARY KEY,
    checksum VARCHAR(64) NOT NULL,
    applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
    execution_ms BIGINT NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
`

// MigrationConfig holds pure database target configuration values injected into the Migrator.
// Architectural Why: Keeps the migration engine completely ignorant of how network
// connectivity (e.g., SSH tunnels, VPNs, or Docker port mappings) is established.
type MigrationConfig struct {
	AppName  string
	EnvName  string
	Version  string
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SkipDrop bool
	SkipSQL  bool
	// DataMigrationsDir is the subdirectory inside dataFS containing versioned *.sql files (default: "migrations").
	DataMigrationsDir string
}

// SchemaExecutor abstracts the underlying DDL diff/execution engine.
// Architectural Why: Decouples the migrator orchestration from how mysqldef is invoked,
// making the Migrator trivially unit-testable with a mock executor without needing a live DB.
type SchemaExecutor interface {
	Execute(ctx context.Context, desiredSQL string, dryRun bool) (string, error)
}

// MysqldefCLIExecutor implements SchemaExecutor by piping the embedded SQL via stdin
// to the mysqldef binary.
type MysqldefCLIExecutor struct {
	cfg MigrationConfig
}

// NewMysqldefCLIExecutor creates a new CLI-backed schema executor.
func NewMysqldefCLIExecutor(cfg MigrationConfig) *MysqldefCLIExecutor {
	return &MysqldefCLIExecutor{cfg: cfg}
}

// Execute runs mysqldef against the configured target database.
func (e *MysqldefCLIExecutor) Execute(ctx context.Context, desiredSQL string, dryRun bool) (string, error) {
	binPath, err := exec.LookPath("mysqldef")
	if err != nil {
		return "", ErrMysqldefNotFound
	}

	args := []string{
		"-h", e.cfg.Host,
		"-P", strconv.Itoa(e.cfg.Port),
		"-u", e.cfg.User,
		e.cfg.Database,
	}
	if dryRun {
		args = append(args, "--dry-run")
	}
	// Architectural Why: In sqldef/mysqldef v2 and v3+, safe additive mode (skipping
	// destructive DROP statements) is the default behavior, and the legacy '--skip-drop'
	// flag was removed. To allow drops when SkipDrop is false, we must pass '--enable-drop'.
	if !e.cfg.SkipDrop {
		args = append(args, "--enable-drop")
	}

	cmd := exec.CommandContext(ctx, binPath, args...)
	// Pass password via environment variable so it never appears in process arguments (ps aux)
	cmd.Env = append(os.Environ(), "MYSQL_PWD="+e.cfg.Password)
	cmd.Stdin = strings.NewReader(desiredSQL)

	var outBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mysqldef execution failed: %w\nOutput:\n%s", err, outBuf.String())
	}

	return outBuf.String(), nil
}

type appliedDataMigration struct {
	Version     string
	Checksum    string
	AppliedAt   time.Time
	ExecutionMs int64
}

type pendingDataMigration struct {
	Version  string
	Checksum string
	SQL      string
}

// Migrator coordinates Phase 1 (declarative schema via mysqldef) and Phase 2 (imperative
// versioned SQL data migrations tracked in schema_migrations).
type Migrator struct {
	schemaFS fs.FS
	dataFS   fs.FS
	executor SchemaExecutor
	cfg      MigrationConfig
	in       io.Reader
	out      io.Writer
}

// NewMigrator enforces constructor injection for all dependencies.
// Note: To adds Phase 2 SQL data migrations, chain .WithDataMigrations(dataFS) or use NewMigratorWithData.
func NewMigrator(schemaFS fs.FS, executor SchemaExecutor, cfg MigrationConfig, in io.Reader, out io.Writer) *Migrator {
	if cfg.AppName == "" {
		cfg.AppName = "DALForge Schema & Data Migrator"
	}
	if cfg.DataMigrationsDir == "" {
		cfg.DataMigrationsDir = "migrations"
	}
	return &Migrator{
		schemaFS: schemaFS,
		executor: executor,
		cfg:      cfg,
		in:       in,
		out:      out,
	}
}

// WithDataMigrations attaches an optional fs.FS containing imperative versioned *.sql scripts
// for Phase 2 execution. Keeping this as a fluent builder method preserves 100% backward
// compatibility with existing NewMigrator callers and unit tests.
func (m *Migrator) WithDataMigrations(dataFS fs.FS) *Migrator {
	m.dataFS = dataFS
	return m
}

// Run executes the Phase 1 (DDL) & Phase 2 (Data SQL) dry-run diff, prompts for a single
// interactive confirmation, and applies both phases in sequence.
func (m *Migrator) Run(ctx context.Context) error {
	desiredSQL, count, err := m.loadEmbeddedSQL()
	if err != nil {
		return err
	}

	envUpper := strings.ToUpper(m.cfg.EnvName)
	fmt.Fprintln(m.out, "================================================================================")
	fmt.Fprintf(m.out, "🐝 %s | Release Version: %s\n", m.cfg.AppName, m.cfg.Version)
	fmt.Fprintf(m.out, "🌍 Environment : %s\n", envUpper)
	fmt.Fprintf(m.out, "📦 Embedded SQL: %d table definitions locked in binary (+ schema_migrations)\n", count)
	fmt.Fprintf(m.out, "🗄️  Target MySQL: %s@%s:%d/%s\n", m.cfg.User, m.cfg.Host, m.cfg.Port, m.cfg.Database)
	fmt.Fprintln(m.out, "================================================================================")

	// -------------------------------------------------------------------------
	// PHASE 1 DRY-RUN: Declarative Schema Diff via mysqldef
	// -------------------------------------------------------------------------
	diffOutput, err := m.executor.Execute(ctx, desiredSQL, true)
	if err != nil {
		return err
	}

	// Architectural Why: mysqldef emits "-- Skipped: DROP TABLE ..." comments when
	// tables from other services (like 'signups') exist in the shared database and
	// safe additive mode (SkipDrop) is active. We separate executable DDL from SQL comments
	// so we don't prompt the operator to apply a no-op migration.
	var executableLines []string
	var skippedLines []string
	for _, line := range strings.Split(diffOutput, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "-- dry run --" || strings.Contains(trimmed, "Nothing is modified") {
			continue
		}
		if strings.HasPrefix(trimmed, "-- Skipped:") {
			skippedLines = append(skippedLines, trimmed)
			continue
		}
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		executableLines = append(executableLines, line)
	}

	if len(skippedLines) > 0 {
		fmt.Fprintf(m.out, "\nℹ️  Ignored/Skipped Statements (%d):\n", len(skippedLines))
		for _, s := range skippedLines {
			fmt.Fprintf(m.out, "   %s\n", s)
		}
	}

	hasSchemaChanges := len(executableLines) > 0
	cleanPlan := strings.TrimSpace(strings.Join(executableLines, "\n"))

	if !hasSchemaChanges {
		fmt.Fprintf(m.out, "\n✅ Phase 1: Database DDL schema on [%s] is 100%% in sync with release [%s]!\n", envUpper, m.cfg.Version)
	} else {
		fmt.Fprintf(m.out, "\n📋 Phase 1: Proposed DDL Changes for [%s] (Release %s):\n", envUpper, m.cfg.Version)
		fmt.Fprintln(m.out, "--------------------------------------------------------------------------------")
		fmt.Fprintln(m.out, cleanPlan)
		fmt.Fprintln(m.out, "--------------------------------------------------------------------------------")

		if strings.Contains(strings.ToUpper(cleanPlan), "DROP ") {
			fmt.Fprintln(m.out, "\n⚠️  WARNING: Destructive (DROP) statements detected in the plan above!")
			fmt.Fprintln(m.out, "   To run only additive changes (CREATE/ADD), pass -skip-drop=true.")
		}
	}

	// -------------------------------------------------------------------------
	// PHASE 2 DRY-RUN: Versioned Imperative SQL Data Migrations
	// -------------------------------------------------------------------------
	var pendingData []pendingDataMigration
	var db *sql.DB

	if m.dataFS != nil && !m.cfg.SkipSQL {
		var err error
		db, pendingData, err = m.prepareDataMigrations(ctx, envUpper)
		if err != nil {
			return err
		}
		defer db.Close()
	}

	hasDataChanges := len(pendingData) > 0

	// If neither Phase 1 nor Phase 2 has pending changes, exit cleanly without prompting.
	if !hasSchemaChanges && !hasDataChanges {
		return nil
	}

	// -------------------------------------------------------------------------
	// UNIFIED INTERACTIVE REVIEW & APPROVAL GATE
	// -------------------------------------------------------------------------
	expectedConfirm := "yes"
	prompt := fmt.Sprintf("\n🚀 Type 'yes' to apply pending migration(s) to [%s]: ", envUpper)
	if envUpper == "PROD" {
		expectedConfirm = "apply-prod"
		prompt = "\n🚨 PRODUCTION WARNING: Type 'apply-prod' to execute pending migration(s) on PROD: "
	}

	fmt.Fprint(m.out, prompt)
	reader := bufio.NewReader(m.in)
	line, err := reader.ReadString('\n')
	if err != nil {
		// Architectural Why: In Ansible's Dry-Run task (`< /dev/null`), stdin immediately
		// returns io.EOF. Returning nil here lets the dry-run task succeed and print the preview.
		if errors.Is(err, io.EOF) && strings.TrimSpace(line) == "" {
			fmt.Fprintln(m.out, "\nℹ️  Dry-run mode detected (EOF). No changes were applied.")
			return nil
		}
		return fmt.Errorf("reading confirmation input: %w", err)
	}

	if strings.TrimSpace(line) != expectedConfirm {
		fmt.Fprintln(m.out, "🛑 Migration aborted by user. No changes were made.")
		return nil
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("migration cancelled: %w", err)
	}

	// -------------------------------------------------------------------------
	// APPLY PHASE 1: Declarative Schema Changes
	// -------------------------------------------------------------------------
	if hasSchemaChanges {
		fmt.Fprintf(m.out, "\n⚡ Applying Phase 1 DDL schema migrations to [%s]...\n", envUpper)
		applyOutput, err := m.executor.Execute(ctx, desiredSQL, false)
		if err != nil {
			return err
		}
		if strings.TrimSpace(applyOutput) != "" {
			fmt.Fprintln(m.out, strings.TrimSpace(applyOutput))
		}
		fmt.Fprintf(m.out, "✅ Phase 1 DDL schema migration for release [%s] successfully applied to [%s]!\n", m.cfg.Version, envUpper)
	}

	// -------------------------------------------------------------------------
	// APPLY PHASE 2: Imperative SQL Data Migrations
	// -------------------------------------------------------------------------
	if hasDataChanges && db != nil {
		if err := m.applyDataMigrations(ctx, db, pendingData); err != nil {
			return err
		}
	}

	return nil
}

// prepareDataMigrations connects to MySQL, ensures the schema_migrations ledger exists,
// verifies SHA-256 checksums of already-applied scripts, and returns pending migrations.
func (m *Migrator) prepareDataMigrations(ctx context.Context, envUpper string) (*sql.DB, []pendingDataMigration, error) {
	fmt.Fprintf(m.out, "\n================================================================================\n")
	fmt.Fprintf(m.out, "📜 Phase 2: Versioned SQL Data Migrations (%s)\n", envUpper)
	fmt.Fprintf(m.out, "================================================================================\n")

	mysqlCfg := mysql.NewConfig()
	mysqlCfg.User = m.cfg.User
	mysqlCfg.Passwd = m.cfg.Password
	mysqlCfg.Net = "tcp"
	mysqlCfg.Addr = fmt.Sprintf("%s:%d", m.cfg.Host, m.cfg.Port)
	mysqlCfg.DBName = m.cfg.Database
	mysqlCfg.ParseTime = true
	mysqlCfg.MultiStatements = true

	db, err := sql.Open("mysql", mysqlCfg.FormatDSN())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open mysql connection for data migrations: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("failed to ping mysql for data migrations: %w", err)
	}

	const createLedgerSQL = `
	CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(255) NOT NULL PRIMARY KEY,
		checksum VARCHAR(64) NOT NULL,
		applied_at DATETIME(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3),
		execution_ms BIGINT NOT NULL
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;`

	if _, err := db.ExecContext(ctx, createLedgerSQL); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("failed to ensure schema_migrations table exists: %w", err)
	}

	applied, err := m.fetchAppliedDataMigrations(ctx, db)
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}

	entries, err := fs.ReadDir(m.dataFS, m.cfg.DataMigrationsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(m.out, "✅ No SQL data migration directory (%s) found.\n", m.cfg.DataMigrationsDir)
			return db, nil, nil
		}
		_ = db.Close()
		return nil, nil, fmt.Errorf("failed to read embedded migrations directory %q: %w", m.cfg.DataMigrationsDir, err)
	}

	var filenames []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			filenames = append(filenames, entry.Name())
		}
	}
	sort.Strings(filenames)

	if len(filenames) == 0 {
		fmt.Fprintf(m.out, "✅ No SQL data migration files found.\n")
		return db, nil, nil
	}

	var pending []pendingDataMigration
	for _, name := range filenames {
		rawBytes, readErr := fs.ReadFile(m.dataFS, m.cfg.DataMigrationsDir+"/"+name)
		if readErr != nil {
			_ = db.Close()
			return nil, nil, fmt.Errorf("failed to read embedded migration %s: %w", name, readErr)
		}

		sqlContent := strings.TrimSpace(string(rawBytes))
		if sqlContent == "" {
			continue
		}

		hash := sha256.Sum256(rawBytes)
		checksum := hex.EncodeToString(hash[:])

		if prev, exists := applied[name]; exists {
			if prev.Checksum != checksum {
				_ = db.Close()
				return nil, nil, fmt.Errorf(
					"🚨 CHECKSUM DRIFT DETECTED for already-applied migration %q!\n   Recorded in DB : %s\n   Current File   : %s\n   Refusing to proceed. Never modify an already-applied migration file",
					name, prev.Checksum, checksum,
				)
			}
			fmt.Fprintf(m.out, "⏭️  [SKIPPED] %s (applied %s, %dms)\n",
				name, prev.AppliedAt.Format("2006-01-02 15:04:05"), prev.ExecutionMs)
			continue
		}

		pending = append(pending, pendingDataMigration{
			Version:  name,
			Checksum: checksum,
			SQL:      sqlContent,
		})
	}

	if len(pending) == 0 {
		fmt.Fprintf(m.out, "\n✅ Phase 2: All SQL data migrations are 100%% in sync!\n")
		return db, nil, nil
	}

	fmt.Fprintf(m.out, "\n📋 PENDING_SQL_DATA_MIGRATIONS: Found %d pending SQL data migration(s):\n", len(pending))
	for i, p := range pending {
		fmt.Fprintf(m.out, "\n--- [%d/%d] %s (sha256: %s) ---\n%s\n",
			i+1, len(pending), p.Version, p.Checksum[:12], p.SQL)
	}

	return db, pending, nil
}

// applyDataMigrations executes each pending SQL data migration inside an atomic transaction
// and records its checksum and duration in the schema_migrations ledger.
func (m *Migrator) applyDataMigrations(ctx context.Context, db *sql.DB, pending []pendingDataMigration) error {
	fmt.Fprintf(m.out, "\n⚡ Applying Phase 2 SQL data migrations (%d pending)...\n", len(pending))

	for _, p := range pending {
		fmt.Fprintf(m.out, "🚀 Applying %s...\n", p.Version)
		start := time.Now()

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin transaction for %s: %w", p.Version, err)
		}

		if _, execErr := tx.ExecContext(ctx, p.SQL); execErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed executing migration %s: %w", p.Version, execErr)
		}

		execMs := time.Since(start).Milliseconds()
		const insertSQL = `INSERT INTO schema_migrations (version, checksum, execution_ms) VALUES (?, ?, ?)`
		if _, logErr := tx.ExecContext(ctx, insertSQL, p.Version, p.Checksum, execMs); logErr != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed recording migration %s in schema_migrations: %w", p.Version, logErr)
		}

		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("failed committing transaction for %s: %w", p.Version, commitErr)
		}

		fmt.Fprintf(m.out, "✅ Applied %s (%dms)\n", p.Version, execMs)
	}

	fmt.Fprintf(m.out, "\n🎉 All pending Phase 2 SQL data migrations applied successfully!\n")
	return nil
}

func (m *Migrator) fetchAppliedDataMigrations(ctx context.Context, db *sql.DB) (map[string]appliedDataMigration, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum, applied_at, execution_ms FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("failed querying schema_migrations ledger: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]appliedDataMigration)
	for rows.Next() {
		var am appliedDataMigration
		if err := rows.Scan(&am.Version, &am.Checksum, &am.AppliedAt, &am.ExecutionMs); err != nil {
			return nil, fmt.Errorf("failed scanning schema_migrations row: %w", err)
		}
		applied[am.Version] = am
	}
	return applied, rows.Err()
}

// loadEmbeddedSQL reads all *.sql files from the injected fs.FS, strips '#' comments,
// and appends the native DALForge schema_migrations ledger table definition.
func (m *Migrator) loadEmbeddedSQL() (string, int, error) {
	files, err := fs.Glob(m.schemaFS, "*.sql")
	if err != nil {
		return "", 0, err
	}
	if len(files) == 0 {
		return "", 0, ErrNoSQLFiles
	}

	sort.Strings(files)

	var b strings.Builder
	for _, file := range files {
		raw, err := fs.ReadFile(m.schemaFS, file)
		if err != nil {
			return "", 0, fmt.Errorf("reading embedded file %s: %w", file, err)
		}

		b.WriteString(fmt.Sprintf("-- Source: %s\n", filepath.Base(file)))
		for _, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}

	// Include the DALForge schema_migrations ledger table in the declarative stream
	b.WriteString(schemaMigrationsDDL)

	return b.String(), len(files), nil
}
