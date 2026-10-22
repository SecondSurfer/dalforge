package dal

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"testing/fstest"
)

type fakeSchemaExecutor struct {
	dryRunOutput string
	applyOutput  string
	err          error
	receivedSQL  string
	applied      bool
}

func (f *fakeSchemaExecutor) Execute(_ context.Context, desiredSQL string, dryRun bool) (string, error) {
	f.receivedSQL = desiredSQL
	if f.err != nil {
		return "", f.err
	}
	if dryRun {
		return f.dryRunOutput, nil
	}
	f.applied = true
	return f.applyOutput, nil
}

func TestMigrator_Run(t *testing.T) {
	mockFS := fstest.MapFS{
		"user.sql": &fstest.MapFile{
			Data: []byte("# Unique indexes\nCREATE TABLE `users` (`id` BIGINT PRIMARY KEY);\n"),
		},
	}

	t.Run("AlreadyInSync_DoesNotPromptOrApply", func(t *testing.T) {
		exec := &fakeSchemaExecutor{dryRunOutput: "-- Nothing is modified --"}
		var out bytes.Buffer
		m := NewMigrator(mockFS, exec, MigrationConfig{
			AppName:  "PurpleHoney Schema Migrator",
			EnvName:  "dev",
			Version:  "v1.0.0",
			Host:     "127.0.0.1",
			Port:     55003,
			User:     "root",
			Database: "purplehoney",
		}, strings.NewReader(""), &out)

		err := m.Run(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exec.applied {
			t.Errorf("expected apply not to be called when schema is already in sync")
		}
		if !strings.Contains(out.String(), "100% in sync") {
			t.Errorf("expected output to confirm sync, got: %s", out.String())
		}
		// Verify '#' comments were stripped from embedded SQL
		if strings.Contains(exec.receivedSQL, "# Unique indexes") {
			t.Errorf("expected '#' comment lines to be stripped, got: %s", exec.receivedSQL)
		}
	})

	t.Run("DevEnvironment_AppliesOnYes", func(t *testing.T) {
		exec := &fakeSchemaExecutor{
			dryRunOutput: "-- dry run --\nALTER TABLE `users` ADD COLUMN `order` TINYINT NOT NULL;",
			applyOutput:  "ALTER TABLE `users` ADD COLUMN `order` TINYINT NOT NULL;",
		}
		var out bytes.Buffer
		m := NewMigrator(mockFS, exec, MigrationConfig{
			EnvName: "dev",
			Version: "v1.0.0",
		}, strings.NewReader("yes\n"), &out)

		err := m.Run(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !exec.applied {
			t.Errorf("expected migration to be applied after 'yes' confirmation")
		}
	})

	t.Run("ProdEnvironment_RequiresApplyProdConfirmation", func(t *testing.T) {
		exec := &fakeSchemaExecutor{
			dryRunOutput: "-- dry run --\nDROP INDEX `idx_old` ON `users`;",
		}
		var out bytes.Buffer
		// Typing "yes" on PROD must abort safely!
		m := NewMigrator(mockFS, exec, MigrationConfig{
			EnvName: "prod",
			Version: "v1.0.0",
		}, strings.NewReader("yes\n"), &out)

		err := m.Run(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if exec.applied {
			t.Errorf("expected PROD migration to abort when user typed 'yes' instead of 'apply-prod'")
		}
		if !strings.Contains(out.String(), "WARNING: Destructive (DROP) statements detected") {
			t.Errorf("expected destructive DROP warning in output, got: %s", out.String())
		}
		if !strings.Contains(out.String(), "Migration aborted by user") {
			t.Errorf("expected abort message in output, got: %s", out.String())
		}

		// Now confirm with 'apply-prod'
		out.Reset()
		m2 := NewMigrator(mockFS, exec, MigrationConfig{
			EnvName: "prod",
			Version: "v1.0.0",
		}, strings.NewReader("apply-prod\n"), &out)

		if err := m2.Run(context.Background()); err != nil {
			t.Fatalf("unexpected error on prod apply: %v", err)
		}
		if !exec.applied {
			t.Errorf("expected PROD migration to apply when user typed 'apply-prod'")
		}
	})

	t.Run("EmptySchemaFS_ReturnsErrNoSQLFiles", func(t *testing.T) {
		emptyFS := fstest.MapFS{}
		exec := &fakeSchemaExecutor{}
		var out bytes.Buffer
		m := NewMigrator(emptyFS, exec, MigrationConfig{EnvName: "dev"}, strings.NewReader(""), &out)

		err := m.Run(context.Background())
		if err != ErrNoSQLFiles {
			t.Errorf("expected ErrNoSQLFiles, got: %v", err)
		}
	})
}
