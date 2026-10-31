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
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	keysStatusDB      string
	keysStatusNoStore bool
)

var serverKeysStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report the server key status from the local keyring file and database",
	Long: `Report the server key status: keyring revision, signing and encryption
ring IDs, emit and write formats, legacy reader lists, signing deadlines,
the formats this binary reads, the database encryption policy and
migration, an authenticated audit of every stored payload (counts only) and
the key-related readiness reasons.

It builds the same report as GET /api/v1/admin/keys/status, from the
keyring file and database configured on this host instead of a running
Hub. The database is opened read only. The endpoint reports the audit the
Hub ran at startup (audit.scope "startup"); this command runs a fresh audit
(audit.scope "fresh"), so it also covers rows written since. The report
never contains key material, ciphertext, payloads or secret record
identifiers.

In legacy mode (no keyring file) the audit uses the session secret from
SCION_SERVER_SESSION_SECRET (or SESSION_SECRET) in this command's
environment; without one, it assumes the Hub also runs without a secret.
A Hub started with --session-secret, or with a different secret, audits
with that secret instead, so the counts can differ.

Use --format json for the versioned JSON report.

Exit codes: 0 a report was produced, whatever it says (a NOT READY report
is still exit 0); 1 the operation failed (the database could not be opened
or read, or the audit failed); 2 invalid configuration.`,
	Args: cobra.NoArgs,
	RunE: runServerKeysStatus,
}

func init() {
	serverKeysCmd.AddCommand(serverKeysStatusCmd)
	addKeysStatusFlags(serverKeysStatusCmd.Flags())
}

// addKeysStatusFlags registers the status flags.
func addKeysStatusFlags(fs *pflag.FlagSet) {
	fs.StringVar(&keysStatusDB, "db", "", "Database DSN (overrides config/env)")
	fs.BoolVar(&keysStatusNoStore, "no-store", false, "Report the keyring only; do not open the database")
}

// keysStatusNow is the report clock; a var for tests.
var keysStatusNow = time.Now

func runServerKeysStatus(cmd *cobra.Command, _ []string) error {
	s, err := loadKeysSettings(cmd)
	if err != nil {
		return err
	}
	k, err := loadKeysKeyring(s)
	if err != nil {
		return err
	}
	c, err := keysValueCipher(k)
	if err != nil {
		return err
	}
	if k == nil {
		printLegacyAuditNote(cmd.ErrOrStderr(), sessionSecretFromEnv() != "")
	}
	ctx := cmd.Context()
	var st store.Store
	if !keysStatusNoStore {
		driver, dsn := resolvePreflightDSN(false, "", keysStatusDB, s.cfg)
		if dsn == "" {
			return keysInvalid(errKeysNoDatabase)
		}
		client, err := openPreflightClient(driver, dsn)
		if err != nil {
			return keysFailed(fmt.Errorf("opening the database read only: %w", err))
		}
		cs := entadapter.NewCompositeStore(client)
		defer func() { _ = cs.Close() }()
		st = cs
	}
	report, err := hub.LocalKeyStatus(ctx, k, c, st, keysStatusNow())
	if err != nil {
		return keysFailed(err)
	}
	out := cmd.OutOrStdout()
	if outputFormat == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return keysFailed(err)
		}
		return nil
	}
	printKeyStatus(out, report)
	return nil
}

// printLegacyAuditNote tells the operator which secret a legacy-mode audit
// uses (stderr, so JSON output stays clean). The Hub may have been started
// with --session-secret or another secret, which this command cannot see.
func printLegacyAuditNote(w io.Writer, haveSecret bool) {
	if haveSecret {
		_, _ = fmt.Fprintf(w, "Note: legacy mode (no keyring file): the audit uses the session secret from %s/%s in this environment; "+
			"a Hub started with --session-secret or another secret can report different counts.\n", sessionSecretEnv, legacySessionSecretEnv)
		return
	}
	_, _ = fmt.Fprintf(w, "Note: legacy mode (no keyring file) and no session secret in this environment (%s/%s): "+
		"this audit assumes the Hub also runs without a session secret; if the Hub was started with --session-secret, counts may differ.\n",
		sessionSecretEnv, legacySessionSecretEnv)
}

// printKeyStatus renders the report for a terminal.
func printKeyStatus(w io.Writer, r hub.KeyStatusReport) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	revision := "(none)"
	if r.Keyring.Revision != nil {
		revision = *r.Keyring.Revision
	}
	p("Key status (report version %d, %s, host %s)", r.ReportVersion, r.Instance.Source, r.Instance.Hostname)
	p("Keyring: source %s, revision %s", r.Keyring.Source, revision)
	p("Signing: active %s, emit %s, accepted %s, legacy readers %s",
		r.Signing.ActiveID, r.Signing.Emit, keysList(r.Signing.AcceptedIDs), keysList(r.Signing.LegacyReadIDs))
	for _, g := range r.Signing.Generations {
		p("  %s: sign_until %s, verify_until %s", g.ID, keysDeadline(g.SignUntil), keysDeadline(g.VerifyUntil))
	}
	p("Encryption: active %s, write_format %s, accepted %s, legacy v1 readers %s",
		r.Encryption.ActiveID, r.Encryption.WriteFormat, keysList(r.Encryption.AcceptedIDs), keysList(r.Encryption.LegacyV1ReadIDs))
	p("Reader formats: signing %s, encryption %s", keysList(r.ReaderFormats.Signing), keysList(r.ReaderFormats.Encryption))
	if r.Policy == nil {
		p("Encryption policy: none")
	} else {
		p("Encryption policy: generation %d, state %s, writers %s, required readers %s",
			r.Policy.Generation, r.Policy.State, keysList(r.Policy.AllowedWriteKIDs), keysList(r.Policy.RequiredReadKIDs))
	}
	if r.Migration != nil {
		p("Migration: %s, state %s, target %s", r.Migration.ID, r.Migration.State, r.Migration.TargetKID)
	}
	if !r.Audit.Ran {
		p("Audit: not run (no database)")
	} else {
		p("Audit (%s, completed %s): %d scanned, %d reference-only, %d readable, %d unreadable (%d infrastructure backups)",
			r.Audit.Scope, keysDeadline(r.Audit.CompletedAt), r.Audit.Scanned, r.Audit.ReferenceOnly, r.Audit.Readable, r.Audit.Unreadable, r.Audit.BackupUnreadable)
		p("  by format: %s; by key ID: %s; unreadable by kind: %s",
			keysCounts(r.Audit.ByFormat), keysCounts(r.Audit.ByKID), keysCounts(r.Audit.UnreadableByKind))
	}
	if len(r.Readiness.Reasons) == 0 {
		p("Readiness: no key-related not-ready reasons")
	} else {
		p("Readiness: NOT READY: %s", strings.Join(r.Readiness.Reasons, ", "))
	}
	p("Material digest: omitted (%s)", r.MaterialDigestOmittedReason)
}

func keysList(s []string) string {
	if len(s) == 0 {
		return "[]"
	}
	return "[" + strings.Join(s, ", ") + "]"
}

// keysDeadline renders a deadline in the display zone (--tz/--utc); JSON
// output keeps RFC 3339.
func keysDeadline(t *time.Time) string {
	if t == nil {
		return "none"
	}
	return clitime.Format(*t, clitime.Full)
}

func keysCounts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, " ")
}
