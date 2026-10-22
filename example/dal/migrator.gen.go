package dal

import (
	"bufio"
	"bytes"
	"context"
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
)

// Domain errors for the schema migration tool.
var (
	ErrMysqldefNotFound = errors.New("mysqldef binary not found in PATH (run: go install github.com/sqldef/sqldef/cmd/mysqldef@latest)")
	ErrNoSQLFiles       = errors.New("no embedded .sql files found in schema filesystem")
)

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
	if e.cfg.SkipDrop {
		args = append(args, "--skip-drop")
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

// Migrator coordinates loading the embedded schema, previewing the diff, and applying changes.
type Migrator struct {
	schemaFS fs.FS
	executor SchemaExecutor
	cfg      MigrationConfig
	in       io.Reader
	out      io.Writer
}

// NewMigrator enforces constructor injection for all dependencies.
func NewMigrator(schemaFS fs.FS, executor SchemaExecutor, cfg MigrationConfig, in io.Reader, out io.Writer) *Migrator {
	if cfg.AppName == "" {
		cfg.AppName = "DALForge Schema Migrator"
	}
	return &Migrator{
		schemaFS: schemaFS,
		executor: executor,
		cfg:      cfg,
		in:       in,
		out:      out,
	}
}

// Run executes the dry-run diff, prompts for interactive confirmation, and applies the migration.
func (m *Migrator) Run(ctx context.Context) error {
	desiredSQL, count, err := m.loadEmbeddedSQL()
	if err != nil {
		return err
	}

	envUpper := strings.ToUpper(m.cfg.EnvName)
	fmt.Fprintln(m.out, "================================================================================")
	fmt.Fprintf(m.out, "🐝 %s | Release Version: %s\n", m.cfg.AppName, m.cfg.Version)
	fmt.Fprintf(m.out, "🌍 Environment : %s\n", envUpper)
	fmt.Fprintf(m.out, "📦 Embedded SQL: %d table definitions locked in binary\n", count)
	fmt.Fprintf(m.out, "🗄️  Target MySQL: %s@%s:%d/%s\n", m.cfg.User, m.cfg.Host, m.cfg.Port, m.cfg.Database)
	fmt.Fprintln(m.out, "================================================================================")

	// 1. Dry-run to compute diff
	diffOutput, err := m.executor.Execute(ctx, desiredSQL, true)
	if err != nil {
		return err
	}

	if strings.Contains(diffOutput, "Nothing is modified") || strings.TrimSpace(diffOutput) == "" {
		fmt.Fprintf(m.out, "\n✅ Database schema on [%s] is 100%% in sync with release [%s]!\n", envUpper, m.cfg.Version)
		return nil
	}

	cleanPlan := strings.TrimSpace(strings.ReplaceAll(diffOutput, "-- dry run --", ""))

	fmt.Fprintf(m.out, "\n📋 Proposed DDL Changes for [%s] (Release %s):\n", envUpper, m.cfg.Version)
	fmt.Fprintln(m.out, "--------------------------------------------------------------------------------")
	fmt.Fprintln(m.out, cleanPlan)
	fmt.Fprintln(m.out, "--------------------------------------------------------------------------------")

	if strings.Contains(strings.ToUpper(cleanPlan), "DROP ") {
		fmt.Fprintln(m.out, "\n⚠️  WARNING: Destructive (DROP) statements detected in the plan above!")
		fmt.Fprintln(m.out, "   To run only additive changes (CREATE/ADD), pass -skip-drop.")
	}

	// 2. Interactive review & approval gate
	expectedConfirm := "yes"
	prompt := fmt.Sprintf("\n🚀 Type 'yes' to apply these changes to [%s]: ", envUpper)
	if envUpper == "PROD" {
		expectedConfirm = "apply-prod"
		prompt = "\n🚨 PRODUCTION WARNING: Type 'apply-prod' to execute these changes on PROD: "
	}

	fmt.Fprint(m.out, prompt)
	reader := bufio.NewReader(m.in)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading confirmation input: %w", err)
	}

	if strings.TrimSpace(line) != expectedConfirm {
		fmt.Fprintln(m.out, "🛑 Migration aborted by user. No changes were made.")
		return nil
	}

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("migration cancelled: %w", err)
	}

	// 3. Apply changes
	fmt.Fprintf(m.out, "\n⚡ Applying schema migrations to [%s]...\n", envUpper)
	applyOutput, err := m.executor.Execute(ctx, desiredSQL, false)
	if err != nil {
		return err
	}
	if strings.TrimSpace(applyOutput) != "" {
		fmt.Fprintln(m.out, strings.TrimSpace(applyOutput))
	}
	fmt.Fprintf(m.out, "✅ Migration for release [%s] successfully applied to [%s]!\n", m.cfg.Version, envUpper)

	return nil
}

// loadEmbeddedSQL reads all *.sql files from the injected fs.FS and strips '#' comments.
// Architectural Why: dalforge templates use '#' for section comments inside .sql files.
// Stripping them here keeps the DDL stream clean for downstream parsers like mysqldef.
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

	return b.String(), len(files), nil
}
