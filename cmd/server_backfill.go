// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/spf13/cobra"
)

var (
	backfillExecute    bool
	backfillProject    string
	backfillBatchSize  int
	backfillCheckpoint string
	backfillDB         string
)

var serverBackfillCmd = &cobra.Command{
	Use:   "backfill",
	Short: "Backfill conversation_id for historical messages",
	Long: `Scan messages that predate the conversation model and assign them
to conversations based on their thread, sender, and recipient metadata.

By default runs in DRY-RUN mode — scans and reports what would change
without modifying the database. Pass --execute to apply changes.

The backfill is idempotent: messages already attributed to a conversation
are skipped, so re-running is safe. It supports resume via --checkpoint.

Examples:
  # Preview what would change (dry-run, all projects):
  scion server backfill

  # Backfill a specific project:
  scion server backfill --project <project-id> --execute

  # Resume an interrupted run:
  scion server backfill --execute --checkpoint <cursor>`,
	RunE: runServerBackfill,
}

func init() {
	serverCmd.AddCommand(serverBackfillCmd)
	serverBackfillCmd.Flags().BoolVar(&backfillExecute, "execute", false, "Apply changes (default: dry-run)")
	serverBackfillCmd.Flags().StringVar(&backfillProject, "project", "", "Backfill a specific project ID (default: all)")
	serverBackfillCmd.Flags().IntVar(&backfillBatchSize, "batch-size", 0, "Messages per batch (0 = default 100)")
	// Note: checkpoint cursors are project-scoped. Using a cursor from one project
	// with a different --project flag may produce incorrect results. When running
	// without --project (all projects), checkpoint is not used (cleared for multi-project runs).
	serverBackfillCmd.Flags().StringVar(&backfillCheckpoint, "checkpoint", "",
		"Resume from this pagination cursor (from a previous run's output; project-scoped)")
	serverBackfillCmd.Flags().StringVar(&backfillDB, "db", "", "Database DSN (overrides config/env)")
}

// backfillConfigFromFlags builds the BackfillConfig from command flags.
// Extracted so the dry-run default is assertable without a database.
func backfillConfigFromFlags() messaging.BackfillConfig {
	return messaging.BackfillConfig{
		DryRun:     !backfillExecute,
		BatchSize:  backfillBatchSize,
		Checkpoint: backfillCheckpoint,
	}
}

func runServerBackfill(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	s, err := openBackfillStore(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	// Determine which projects to backfill.
	var projectIDs []string
	if backfillProject != "" {
		projectIDs = []string{backfillProject}
	} else {
		cursor := ""
		for {
			projects, err := s.ListProjects(ctx, store.ProjectFilter{}, store.ListOptions{Limit: 500, Cursor: cursor})
			if err != nil {
				return fmt.Errorf("listing projects: %w", err)
			}
			for _, p := range projects.Items {
				projectIDs = append(projectIDs, p.ID)
			}
			if projects.NextCursor == "" {
				break
			}
			cursor = projects.NextCursor
		}
		if len(projectIDs) == 0 {
			_, _ = fmt.Fprintln(out, "No projects found.")
			return nil
		}
	}

	// Build the backfill config once; per-project runs share it.
	cfg := backfillConfigFromFlags()

	// Aggregate results across all projects.
	total := &messaging.BackfillResult{}

	for _, pid := range projectIDs {
		cfg.ProjectID = pid
		result, err := runBackfillWithStore(ctx, s, cfg)
		if err != nil {
			return fmt.Errorf("backfill project %s: %w", pid, err)
		}
		mergeBackfillResult(total, result)
	}

	// Checkpoint is only meaningful for single-project runs.
	if len(projectIDs) > 1 {
		total.LastCheckpoint = ""
	}

	// Print report.
	printBackfillReport(out, total, projectIDs)

	if len(total.Errors) > 0 {
		return fmt.Errorf("backfill completed with %d error(s)", len(total.Errors))
	}
	return nil
}

// runBackfillWithStore is the testable core: given a store and config, run
// the backfill for a single project and return the result.
func runBackfillWithStore(ctx context.Context, s store.Store, cfg messaging.BackfillConfig) (*messaging.BackfillResult, error) {
	svc := messaging.NewBackfillService(s, s, s)
	return svc.Run(ctx, cfg)
}

// openBackfillStore resolves the database DSN and returns a CompositeStore.
// Precedence: --db flag > config file (via LoadGlobalConfig).
func openBackfillStore(ctx context.Context) (*entadapter.CompositeStore, error) {
	cfg, err := config.LoadGlobalConfig(serverConfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	// --db flag overrides config.
	if backfillDB != "" {
		cfg.Database.URL = backfillDB
		// Auto-detect driver from DSN.
		if strings.HasPrefix(backfillDB, "postgres://") || strings.HasPrefix(backfillDB, "postgresql://") {
			cfg.Database.Driver = "postgres"
		} else {
			cfg.Database.Driver = "sqlite"
		}
	}

	if cfg.Database.URL == "" {
		return nil, fmt.Errorf("no database configured: set --db, SCION_SERVER_DATABASE_URL env, or database.url in server config")
	}

	var s *entadapter.CompositeStore

	switch cfg.Database.Driver {
	case "sqlite":
		dsn := cfg.Database.URL
		if !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + dsn
		}
		if !strings.Contains(dsn, "?") {
			dsn += "?cache=shared"
		} else if !strings.Contains(dsn, "cache=") {
			dsn += "&cache=shared"
		}
		client, err := entc.OpenSQLite(dsn, entc.PoolConfig{})
		if err != nil {
			return nil, fmt.Errorf("opening sqlite: %w", err)
		}
		if err := entc.AutoMigrate(ctx, client); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("running migrations: %w", err)
		}
		s = entadapter.NewCompositeStore(client)

	case "postgres":
		client, err := entc.OpenPostgres(cfg.Database.URL, entc.PoolConfig{MaxOpenConns: 10, MaxIdleConns: 5})
		if err != nil {
			return nil, fmt.Errorf("opening postgres (verify DSN and network connectivity): %w", err)
		}
		if err := entc.AutoMigrate(ctx, client); err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("running migrations: %w", err)
		}
		s = entadapter.NewCompositeStore(client)

	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}

	return s, nil
}

// mergeBackfillResult adds the counts from src into dst.
func mergeBackfillResult(dst, src *messaging.BackfillResult) {
	dst.TotalProcessed += src.TotalProcessed
	dst.Attributed += src.Attributed
	dst.Inferred += src.Inferred
	dst.Skipped += src.Skipped
	dst.ConversationsCreated += src.ConversationsCreated
	dst.HazardAEmailCount += src.HazardAEmailCount
	dst.HazardBSlugCount += src.HazardBSlugCount
	if src.LastCheckpoint != "" {
		dst.LastCheckpoint = src.LastCheckpoint
	}
	dst.Errors = append(dst.Errors, src.Errors...)
}

// printBackfillReport writes a human-readable summary to out.
func printBackfillReport(out io.Writer, r *messaging.BackfillResult, projectIDs []string) {
	mode := "dry-run"
	if backfillExecute {
		mode = "execute"
	}

	projectLabel := backfillProject
	if projectLabel == "" {
		projectLabel = fmt.Sprintf("all (%d projects)", len(projectIDs))
	}

	_, _ = fmt.Fprintln(out, "Conversation Backfill Report")
	_, _ = fmt.Fprintln(out, "============================")
	_, _ = fmt.Fprintf(out, "Mode:                  %s\n", mode)
	_, _ = fmt.Fprintf(out, "Project:               %s\n", projectLabel)
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Messages processed:    %d\n", r.TotalProcessed)
	_, _ = fmt.Fprintf(out, "  Attributed:          %d\n", r.Attributed)
	_, _ = fmt.Fprintf(out, "  Inferred (hazard-a): %d\n", r.Inferred)
	_, _ = fmt.Fprintf(out, "  Skipped:             %d\n", r.Skipped)
	_, _ = fmt.Fprintf(out, "Conversations created: %d\n", r.ConversationsCreated)
	_, _ = fmt.Fprintf(out, "Hazard-b (slug refs):  %d\n", r.HazardBSlugCount)
	_, _ = fmt.Fprintf(out, "Errors:                %d\n", len(r.Errors))
	if r.LastCheckpoint != "" {
		_, _ = fmt.Fprintf(out, "Last checkpoint:       %s\n", r.LastCheckpoint)
	}
	if len(projectIDs) > 1 {
		_, _ = fmt.Fprintln(out, "  (checkpoint valid for single-project runs only)")
	}

	if len(r.Errors) > 0 {
		_, _ = fmt.Fprintln(out)
		_, _ = fmt.Fprintln(out, "Errors:")
		for _, e := range r.Errors {
			_, _ = fmt.Fprintf(out, "  - %s\n", e)
		}
	}
}
