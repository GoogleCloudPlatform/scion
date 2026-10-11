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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/entc"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/testinfra"
	"github.com/spf13/cobra"
)

// testInfraMarkAcknowledgeFlag must be given to `scion server test-infra
// mark`. Its name states what designating a database as test
// infrastructure allows.
const testInfraMarkAcknowledgeFlag = "i-understand-this-hub-can-mint-super-admins"

var (
	testInfraMarkHubID       string
	testInfraMarkAcknowledge bool
	testInfraMarkAttested    string
	testInfraMarkOperator    string
	testInfraMarkConfigPath  string
	testInfraMarkDBURL       string

	testInfraUnmarkHubID       string
	testInfraUnmarkAcknowledge bool
	testInfraUnmarkOperator    string
	testInfraUnmarkConfigPath  string
	testInfraUnmarkDBURL       string
)

// serverTestInfraCmd groups the offline test-infrastructure commands.
var serverTestInfraCmd = &cobra.Command{
	Use:   "test-infra",
	Short: "Offline test-infrastructure designation for test hubs",
	Long: `Offline commands that designate a hub database as test infrastructure.

These commands run on the hub host against the configured database. They are
not available through any hub API.`,
}

// serverTestInfraMarkCmd writes the test infrastructure marker (test admin
// tier gate condition G6).
var serverTestInfraMarkCmd = &cobra.Command{
	Use:   "mark",
	Short: "Designate this hub's database as test infrastructure",
	Long: `Write the test infrastructure marker into the configured hub database.

The marker records that an operator deliberately designated this database as
the test database of one hub. --enable-test-admin-identities refuses to start
unless the marker is present and names the hub's own hub id.

The command:
  - opens the configured database directly (no running server is needed)
  - writes one marker row (hub id, class "test", time, host, attested JSON)
  - generates the hub-local test-identity web session key (64 random bytes)
    and stores it in the marker row; it is never printed
  - writes a test_infra_mark mutation audit row in the same transaction
  - refuses if a marker is already present (run "unmark" first; marking
    again generates a new web session key, which ends every test-identity
    browser session)

--attested records operator-declared substrate facts as a JSON object, for
example the storage bucket, secret-backend project and broker ids. They are
recorded as declared, not verified. Never put credentials in it.

Example:
  scion server test-infra mark --hub-id <hub-id> \
    --` + testInfraMarkAcknowledgeFlag + ` \
    --attested '{"bucket":"<bucket>","secret_project":"<project>","broker_ids":["<id>"]}'`,
	Args: testInfraMarkArgs,
	RunE: runTestInfraMark,
}

// serverTestInfraUnmarkCmd deletes the test infrastructure marker and, with
// it, the test-identity web session key.
var serverTestInfraUnmarkCmd = &cobra.Command{
	Use:   "unmark",
	Short: "Remove this hub database's test infrastructure designation",
	Long: `Delete the test infrastructure marker from the configured hub database.

The command:
  - opens the configured database directly (no running server is needed)
  - deletes the marker row, and with it the hub-local test-identity web
    session key, so every test-identity browser session cookie stops working
  - writes a test_infra_unmark mutation audit row in the same transaction
  - fails if no marker is present

Restart the hub afterwards: it reads the marker only at startup.

Example:
  scion server test-infra unmark --hub-id <hub-id> --` + testInfraMarkAcknowledgeFlag,
	Args: testInfraUnmarkArgs,
	RunE: runTestInfraUnmark,
}

func init() {
	serverCmd.AddCommand(serverTestInfraCmd)
	serverTestInfraCmd.AddCommand(serverTestInfraMarkCmd)
	serverTestInfraCmd.AddCommand(serverTestInfraUnmarkCmd)

	uf := serverTestInfraUnmarkCmd.Flags()
	uf.StringVar(&testInfraUnmarkHubID, "hub-id", "", "Hub id the marker must name (the hub's resolved hub id)")
	uf.BoolVar(&testInfraUnmarkAcknowledge, testInfraMarkAcknowledgeFlag, false, "Required acknowledgement")
	uf.StringVar(&testInfraUnmarkOperator, "operator", "", "Operator identity for the audit record (defaults to $USER@hostname)")
	uf.StringVar(&testInfraUnmarkConfigPath, "config", "", "Path to server configuration file")
	uf.StringVar(&testInfraUnmarkDBURL, "db", "", "Database URL/path (overrides config)")

	f := serverTestInfraMarkCmd.Flags()
	f.StringVar(&testInfraMarkHubID, "hub-id", "", "Hub id the database is designated for (the hub's resolved hub id)")
	f.BoolVar(&testInfraMarkAcknowledge, testInfraMarkAcknowledgeFlag, false, "Required acknowledgement")
	f.StringVar(&testInfraMarkAttested, "attested", "", "Operator-attested substrate facts as a JSON object (declared, not verified; no credentials)")
	f.StringVar(&testInfraMarkOperator, "operator", "", "Operator identity for the audit record (defaults to $USER@hostname)")
	f.StringVar(&testInfraMarkConfigPath, "config", "", "Path to server configuration file")
	f.StringVar(&testInfraMarkDBURL, "db", "", "Database URL/path (overrides config)")
}

// testInfraMarkArgs validates the mark flags before anything is opened.
func testInfraMarkArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(args, " "))
	}
	if testInfraMarkHubID == "" {
		return errors.New("--hub-id is required")
	}
	if strings.TrimSpace(testInfraMarkHubID) != testInfraMarkHubID {
		return errors.New("--hub-id must not carry surrounding whitespace")
	}
	if !testInfraMarkAcknowledge {
		return fmt.Errorf("--%s is required", testInfraMarkAcknowledgeFlag)
	}
	if testInfraMarkAttested != "" {
		if _, err := parseAttestedJSON(testInfraMarkAttested); err != nil {
			return err
		}
	}
	return nil
}

// parseAttestedJSON accepts a JSON object and returns it compacted.
func parseAttestedJSON(raw string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || obj == nil {
		return nil, errors.New("--attested must be a JSON object")
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, errors.New("--attested must be a JSON object")
	}
	return out, nil
}

func runTestInfraMark(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()
	out := cmd.OutOrStdout()

	cfg, client, err := openTestInfraDatabase(ctx, testInfraMarkConfigPath, testInfraMarkDBURL)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	return markTestInfra(ctx, client, cfg, out)
}

// openTestInfraDatabase loads the server configuration, applies a --db
// override, and opens (and migrates) the configured database.
func openTestInfraDatabase(ctx context.Context, configPath, dbURL string) (*config.GlobalConfig, *ent.Client, error) {
	cfg, err := config.LoadGlobalConfig(configPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load configuration: %w", err)
	}
	if dbURL != "" {
		if strings.HasPrefix(dbURL, "postgres://") || strings.HasPrefix(dbURL, "postgresql://") || strings.Contains(dbURL, "host=") {
			cfg.Database.Driver = "postgres"
		} else {
			cfg.Database.Driver = "sqlite"
		}
		cfg.Database.URL = dbURL
	}
	if cfg.Database.URL == "" {
		return nil, nil, errors.New("no database URL configured; provide --db or a server configuration")
	}
	client, err := openTestInfraClient(ctx, cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to open database: %w", err)
	}
	return cfg, client, nil
}

// testInfraUnmarkArgs validates the unmark flags before anything is opened.
func testInfraUnmarkArgs(_ *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(args, " "))
	}
	if testInfraUnmarkHubID == "" {
		return errors.New("--hub-id is required")
	}
	if !testInfraUnmarkAcknowledge {
		return fmt.Errorf("--%s is required", testInfraMarkAcknowledgeFlag)
	}
	return nil
}

func runTestInfraUnmark(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()

	_, client, err := openTestInfraDatabase(ctx, testInfraUnmarkConfigPath, testInfraUnmarkDBURL)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	return unmarkTestInfra(ctx, client, cmd.OutOrStdout())
}

// unmarkTestInfra deletes the marker through pkg/testinfra after checking
// that it names --hub-id, and reports it.
func unmarkTestInfra(ctx context.Context, client *ent.Client, out io.Writer) error {
	m, err := testinfra.Read(ctx, client)
	if err != nil {
		return err
	}
	if m.HubID != testInfraUnmarkHubID {
		return fmt.Errorf("the test infrastructure marker names hub id %q, not --hub-id %q; nothing was changed", m.HubID, testInfraUnmarkHubID)
	}
	operator := testInfraUnmarkOperator
	if operator == "" {
		operator = resolveOperatorIdentity()
	}
	host, _ := os.Hostname()
	if _, err := testinfra.Unmark(ctx, client, testinfra.UnmarkInput{Operator: operator, Host: host, Now: time.Now()}); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Test infrastructure marker deleted: hub_id=%s. The test-identity web session key is destroyed; restart the hub.\n", m.HubID)
	return nil
}

// markTestInfra writes the marker through pkg/testinfra and reports it.
func markTestInfra(ctx context.Context, client *ent.Client, cfg *config.GlobalConfig, out io.Writer) error {
	var attested json.RawMessage
	if testInfraMarkAttested != "" {
		a, err := parseAttestedJSON(testInfraMarkAttested)
		if err != nil {
			return err
		}
		attested = a
	}
	operator := testInfraMarkOperator
	if operator == "" {
		operator = resolveOperatorIdentity()
	}
	host, _ := os.Hostname()

	// The hub resolves its id from server.hub.hub_id or the environment;
	// warn when this host would resolve a different one (read-only: never
	// creates ~/.scion/hub-id).
	resolved := cfg.Hub.HubID
	if resolved == "" {
		if id, ok := config.ResolveHubIDFromEnvReadOnly(); ok {
			resolved = id
		}
	}
	if resolved != "" && resolved != testInfraMarkHubID {
		_, _ = fmt.Fprintf(out, "WARNING: --hub-id %q differs from the hub id this host resolves (%q); the gate requires the hub's own id.\n", testInfraMarkHubID, resolved)
	}

	m, err := testinfra.Mark(ctx, client, testinfra.MarkInput{
		HubID:    testInfraMarkHubID,
		Operator: operator,
		Host:     host,
		Attested: attested,
		Now:      time.Now(),
	})
	if err != nil {
		if errors.Is(err, testinfra.ErrMarkerExists) {
			return fmt.Errorf("%w; remove the existing marker row on the host before marking again", err)
		}
		return err
	}
	_, _ = fmt.Fprintf(out, "Test infrastructure marker written: hub_id=%s class=%s created_at=%s host=%s\n",
		m.HubID, m.Class, m.CreatedAt.UTC().Format(time.RFC3339), m.CreatedByHost)
	_, _ = fmt.Fprintln(out, "A new test-identity web session key was generated and stored with the marker (not shown).")
	_, _ = fmt.Fprintln(out, "If this hub predates hub-id signing-key scoping, delete or rotate its hub signing keys before the first start with --enable-test-admin-identities.")
	return nil
}

// openTestInfraClient opens the configured database and migrates it.
func openTestInfraClient(ctx context.Context, cfg *config.GlobalConfig) (*ent.Client, error) {
	pool := entc.PoolConfig{MaxOpenConns: 2, MaxIdleConns: 2}
	var client *ent.Client
	var err error
	switch strings.ToLower(cfg.Database.Driver) {
	case "sqlite", "":
		dsn := cfg.Database.URL
		if !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + dsn
		}
		if !strings.Contains(dsn, "?") {
			dsn += "?cache=shared"
		} else if !strings.Contains(dsn, "cache=") {
			dsn += "&cache=shared"
		}
		client, err = entc.OpenSQLite(dsn, pool)
	case "postgres":
		client, err = entc.OpenPostgres(cfg.Database.URL, pool)
	default:
		return nil, fmt.Errorf("unsupported database driver: %s", cfg.Database.Driver)
	}
	if err != nil {
		return nil, err
	}
	// The same migration entry point as the server and recover-authz.
	if err := entadapter.NewCompositeStore(client).Migrate(ctx); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("failed to run database migration: %w", err)
	}
	return client, nil
}

// loadTestIdentityWebSessionKey reads the hub-local test identity web
// session key from the test infrastructure marker at startup (WS1, DEC-8).
// It never fails startup: with test identities off, no database, or a
// missing, mismatched or malformed key, it returns no key and the reason,
// and test identity web sessions are unavailable. It never falls back to a
// key derived from the shared session secret, and never logs the key.
func loadTestIdentityWebSessionKey(ctx context.Context, client *ent.Client, hubID string, enabled bool) hub.TestIdentityWebSessionKey {
	if !enabled {
		return hub.TestIdentityWebSessionKey{Unavailable: "test_identities_disabled"}
	}
	if client == nil {
		return hub.TestIdentityWebSessionKey{Unavailable: "no_database"}
	}
	key, err := testinfra.ReadWebSessionKey(ctx, client, hubID)
	switch {
	case err == nil:
		return hub.TestIdentityWebSessionKey{Key: key}
	case errors.Is(err, testinfra.ErrMarkerAbsent):
		return hub.TestIdentityWebSessionKey{Unavailable: "marker_absent"}
	case errors.Is(err, testinfra.ErrWebSessionKeyHubMismatch):
		return hub.TestIdentityWebSessionKey{Unavailable: "marker_hub_mismatch"}
	case errors.Is(err, testinfra.ErrWebSessionKeyClass):
		return hub.TestIdentityWebSessionKey{Unavailable: "marker_class_not_test"}
	case errors.Is(err, testinfra.ErrWebSessionKeyMissing):
		return hub.TestIdentityWebSessionKey{Unavailable: "key_missing"}
	case errors.Is(err, testinfra.ErrWebSessionKeyInvalid):
		return hub.TestIdentityWebSessionKey{Unavailable: "key_invalid"}
	default:
		return hub.TestIdentityWebSessionKey{Unavailable: "marker_unreadable"}
	}
}
