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
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	keysVerifyDB         string
	keysVerifyReportFile string
)

var serverKeysVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Verify that every stored secret payload authenticates under an allowed enc:v2 key",
	Long: `Verify the stored secret payloads in the database configured on this host.

Every row that holds a local payload is decrypted and authenticated with the
keyring file's encryption ring (in legacy mode, the session secret in this
command's environment), and Hub infrastructure backups are structurally
validated. A row's format and key ID are those of the key that actually
authenticated it, never its envelope label: a value labelled with an allowed
key ID that only opens under another key, or under none, fails.

Allowed writer key IDs: the database encryption policy's allowed_write_kids
when a policy exists. Without a policy, the keyring file's active encryption
key when its write_format is v2; this release refuses a keyring file with
write_format v2, so that case is not reachable yet. In legacy mode (no
keyring file), or with write_format v1 and no policy, the allowed set is
empty, so every row that holds a local payload fails.

Row handling:
  - a reference-only row (empty value, backend reference) holds no local
    payload; it passes and is never decrypted;
  - a row with an empty value and no reference is read as plaintext, so it
    fails; re-encrypt it, give it a backend reference, or delete it;
  - every other row passes only if it authenticates in enc:v2 under an
    allowed key ID; plaintext, v1, a v2 key outside the allowed set, an
    unreadable value and an invalid infrastructure backup all fail.

The database is opened read only and no lock is taken: the result is a
point-in-time check, not a fence against concurrent writes.

Output is aggregate counts only (--format json for JSON). Record identifiers
(ID, key, scope, scope ID and failure reason; never payloads) are written
only to --report-file, a new file created with owner-only permissions; an
existing path or a symlink is refused.

Exit codes: 0 every row passed; 1 at least one row failed, or the operation
failed (the database could not be opened or read); 2 invalid configuration
(including an unusable --report-file path); nothing was checked.`,
	Args: cobra.NoArgs,
	RunE: runServerKeysVerify,
}

func init() {
	serverKeysCmd.AddCommand(serverKeysVerifyCmd)
	addKeysVerifyFlags(serverKeysVerifyCmd.Flags())
}

// addKeysVerifyFlags registers the verify flags.
func addKeysVerifyFlags(fs *pflag.FlagSet) {
	fs.StringVar(&keysVerifyDB, "db", "", "Database DSN (overrides config/env)")
	fs.StringVar(&keysVerifyReportFile, "report-file", "", "Write the failing record identifiers to this new private file (never overwritten)")
}

// errKeysVerifyFailed is the exit-1 error of a verification with failing
// rows.
var errKeysVerifyFailed = errors.New("encryption verification failed: some stored payloads are not readable enc:v2 values under an allowed key ID")

func runServerKeysVerify(cmd *cobra.Command, _ []string) error {
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
	driver, dsn := resolvePreflightDSN(false, "", keysVerifyDB, s.cfg)
	if dsn == "" {
		return keysInvalid(errKeysNoDatabase)
	}
	var report *verifyReportFile
	if keysVerifyReportFile != "" {
		if report, err = createVerifyReportFile(keysVerifyReportFile); err != nil {
			return keysInvalid(err)
		}
	}
	// abandon removes an unfinished report file; a no-op once written.
	defer report.abandon()
	if k == nil {
		printLegacyAuditNote(cmd.ErrOrStderr(), sessionSecretFromEnv() != "")
	}
	client, err := openPreflightClient(driver, dsn)
	if err != nil {
		return keysFailed(fmt.Errorf("opening the database read only: %w", err))
	}
	cs := entadapter.NewCompositeStore(client)
	defer func() { _ = cs.Close() }()

	result, err := hub.VerifyEncryption(cmd.Context(), cs, c)
	if err != nil {
		return keysFailed(err)
	}
	if report != nil {
		if err := report.write(result.Failures); err != nil {
			return keysFailed(err)
		}
	}
	out := cmd.OutOrStdout()
	if outputFormat == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			return keysFailed(err)
		}
	} else {
		printKeysVerify(out, result, keysVerifyReportFile)
	}
	if !result.OK {
		return keysFailed(errKeysVerifyFailed)
	}
	return nil
}

// printKeysVerify renders the aggregate result for a terminal: counts
// only, never a record identifier.
func printKeysVerify(w io.Writer, r hub.EncryptionVerifyReport, reportPath string) {
	p := func(format string, a ...any) { _, _ = fmt.Fprintf(w, format+"\n", a...) }
	p("Allowed writer key IDs (%s): %s", r.AllowedSource, keysList(r.AllowedWriteKIDs))
	p("Scanned %d: %d passed (%d reference-only, not decrypted), %d failed (%d empty without a reference)",
		r.Scanned, r.Passed, r.ReferenceOnly, r.Failed, r.EmptyNoRef)
	p("  failed by reason: %s", keysCounts(r.FailedByReason))
	if reportPath != "" {
		p("Failing record identifiers: %d written to %s", len(r.Failures), reportPath)
	}
	if r.OK {
		p("Verify: OK")
	} else {
		p("Verify: FAILED")
	}
}

// verifyReportFile is the private --report-file, created before the scan
// so an unusable path is refused before any work.
type verifyReportFile struct {
	t    *privatefs.Tree
	f    *os.File
	done bool
}

// createVerifyReportFile creates path as a new private file. It refuses
// an existing path and a symlink: the Lstat pre-check names the reason,
// and the create itself (privatefs: O_EXCL, never following a link at the
// leaf) refuses one planted after the check.
func createVerifyReportFile(path string) (*verifyReportFile, error) {
	return createVerifyReportFileWith(path, os.Lstat)
}

// createVerifyReportFileWith is createVerifyReportFile with its pre-check
// lstat as a seam for tests.
func createVerifyReportFileWith(path string, lstat func(string) (os.FileInfo, error)) (*verifyReportFile, error) {
	if fi, err := lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing --report-file %s: it is a symlink; name a new file", path)
		}
		return nil, fmt.Errorf("refusing --report-file %s: it already exists; name a new file", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("cannot use --report-file %s: %w", path, err)
	}
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{})
	if err != nil {
		return nil, fmt.Errorf("cannot create --report-file %s: %w", path, err)
	}
	f, err := tree.Create(filepath.Base(path), true, privatefs.Private)
	if err != nil {
		_ = tree.Close()
		if errors.Is(err, os.ErrExist) || errors.Is(err, privatefs.ErrReparsePoint) {
			return nil, fmt.Errorf("refusing --report-file %s: it already exists; name a new file", path)
		}
		return nil, fmt.Errorf("cannot create --report-file %s: %w", path, err)
	}
	return &verifyReportFile{t: tree, f: f}, nil
}

// write writes one JSON object per failing record and closes the file.
func (r *verifyReportFile) write(failures []hub.VerifyFailure) error {
	enc := json.NewEncoder(r.f)
	for _, fl := range failures {
		if err := enc.Encode(fl); err != nil {
			return fmt.Errorf("writing --report-file: %w", err)
		}
	}
	if err := r.f.Close(); err != nil {
		return fmt.Errorf("writing --report-file: %w", err)
	}
	r.done = true
	_ = r.t.Close()
	return nil
}

// abandon removes the report file through its open handle unless it was
// written; nil-safe.
func (r *verifyReportFile) abandon() {
	if r == nil || r.done {
		return
	}
	_ = r.t.RemoveOpen(r.f)
	_ = r.f.Close()
	_ = r.t.Close()
	r.done = true
}
