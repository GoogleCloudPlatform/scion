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

//go:build !no_sqlite

package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// faultSMClient is a mockSMClient whose reads and writes can be made to
// fail, or whose reads can return a substituted value, per secret path.
type faultSMClient struct {
	*mockSMClient
	fmu        sync.Mutex
	accessData map[string][]byte // full secret path -> substituted read-back
	accessErr  map[string]error  // full secret path -> read-back error
	addErr     map[string]error  // full secret path -> AddSecretVersion error
	adds       int               // AddSecretVersion calls
}

func trimVersion(name string) string {
	if i := strings.Index(name, "/versions/"); i >= 0 {
		return name[:i]
	}
	return name
}

func (f *faultSMClient) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	path := trimVersion(req.Name)
	f.fmu.Lock()
	err, hasErr := f.accessErr[path]
	data, hasData := f.accessData[path]
	f.fmu.Unlock()
	if hasErr {
		return nil, err
	}
	if hasData {
		return &smpb.AccessSecretVersionResponse{Name: req.Name, Payload: &smpb.SecretPayload{Data: data}}, nil
	}
	return f.mockSMClient.AccessSecretVersion(ctx, req)
}

func (f *faultSMClient) AddSecretVersion(ctx context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	f.fmu.Lock()
	err, hasErr := f.addErr[req.Parent]
	f.adds++
	f.fmu.Unlock()
	if hasErr {
		return nil, err
	}
	return f.mockSMClient.AddSecretVersion(ctx, req)
}

func (f *faultSMClient) clearFaults() {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.accessData = map[string][]byte{}
	f.accessErr = map[string]error{}
	f.addErr = map[string]error{}
}

// migrationFixture is a Hub database whose secrets are written by a
// LocalBackend with a real (legacy-mode, encrypting) cipher, and a
// GCPBackend over the same database with a fault-injecting mock Secret
// Manager.
type migrationFixture struct {
	st     store.Store
	cipher *ValueCipher
	local  *LocalBackend
	gcp    *GCPBackend
	sm     *faultSMClient
}

const migScopeID = "user-1"

func newMigrationFixture(t *testing.T) *migrationFixture {
	t.Helper()
	return newMigrationFixtureWith(t, NewLegacyValueCipher("migration-test-shared-secret"))
}

func newMigrationFixtureWith(t *testing.T, c *ValueCipher) *migrationFixture {
	t.Helper()
	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	sm := &faultSMClient{mockSMClient: newMockSMClient()}
	sm.clearFaults()
	return &migrationFixture{
		st:     st,
		cipher: c,
		local:  NewLocalBackendWithCipher(st, "test-hub-id", c),
		gcp:    NewGCPBackendWithClient(st, sm, "test-project", "test-hub-id"),
		sm:     sm,
	}
}

// seed stores value for name in user scope through the LocalBackend, checks
// that the row holds an encrypted at-rest form, and returns that form.
func (f *migrationFixture) seed(t *testing.T, name, value string) string {
	t.Helper()
	return f.seedAs(t, name, value, store.SecretTypeEnvironment, store.ScopeUser, migScopeID)
}

func (f *migrationFixture) seedAs(t *testing.T, name, value, secretType, scope, scopeID string) string {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.local.Set(ctx, &SetSecretInput{
		Name: name, Value: value, SecretType: secretType, Scope: scope, ScopeID: scopeID,
	}); err != nil {
		t.Fatalf("seeding %s: %v", name, err)
	}
	raw, err := f.st.GetSecretValue(ctx, name, scope, scopeID)
	if err != nil {
		t.Fatalf("reading seeded %s: %v", name, err)
	}
	if raw == value || raw == "" {
		t.Fatalf("precondition: %s must be stored encrypted at rest", name)
	}
	return raw
}

// path is the full Secret Manager path the backend writes name to.
func (f *migrationFixture) path(name, scope, scopeID string) string {
	return "projects/test-project/secrets/" + f.gcp.gcpSecretName(name, scope, scopeID)
}

// smValue is the latest Secret Manager value written for name in user
// scope, and whether one exists.
func (f *migrationFixture) smValue(name string) (string, bool) {
	f.sm.mu.Lock()
	defer f.sm.mu.Unlock()
	v, ok := f.sm.versions[f.path(name, store.ScopeUser, migScopeID)]
	return string(v), ok
}

func (f *migrationFixture) run(t *testing.T, dryRun bool) (MigrationReport, string, error) {
	t.Helper()
	var log bytes.Buffer
	report, err := MigrateLocalToGCP(context.Background(), f.st, f.cipher, f.gcp, dryRun, &log)
	return report, log.String(), err
}

// assertKeptLocal checks that name's row is untouched: no Secret Manager
// reference, the same stored form as before, and still readable through
// the local backend (so under the local key).
func (f *migrationFixture) assertKeptLocal(t *testing.T, name, rawBefore, plain string) {
	t.Helper()
	ctx := context.Background()
	row, err := f.st.GetSecret(ctx, name, store.ScopeUser, migScopeID)
	if err != nil {
		t.Fatalf("GetSecret %s: %v", name, err)
	}
	if row.SecretRef != "" {
		t.Errorf("%s: row was marked migrated (has a reference)", name)
	}
	raw, err := f.st.GetSecretValue(ctx, name, store.ScopeUser, migScopeID)
	if err != nil {
		t.Fatalf("GetSecretValue %s: %v", name, err)
	}
	if raw != rawBefore {
		t.Errorf("%s: stored local form changed", name)
	}
	sv, err := f.local.Get(ctx, name, store.ScopeUser, migScopeID)
	if err != nil {
		t.Fatalf("%s: no longer readable under the local key: %v", name, err)
	}
	if sv.Value != plain {
		t.Errorf("%s: local read returned a different value", name)
	}
}

// assertNoLeak fails when any of the outputs contains any of the
// sensitive strings. It never prints a sensitive string itself.
func assertNoLeak(t *testing.T, report MigrationReport, err error, log string, sensitive ...string) {
	t.Helper()
	rj, mErr := json.Marshal(report)
	if mErr != nil {
		t.Fatalf("marshal report: %v", mErr)
	}
	outputs := map[string]string{"log": log, "report": string(rj)}
	if err != nil {
		outputs["error"] = err.Error()
	}
	for where, text := range outputs {
		for i, s := range sensitive {
			if s != "" && strings.Contains(text, s) {
				t.Errorf("%s contains sensitive string #%d", where, i)
			}
		}
	}
}

func TestMigrateLocalToGCP_WritesDecodedValue(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-api-key-value-1"
	raw := f.seed(t, "API_KEY", plain)

	report, log, err := f.run(t, false)
	if err != nil {
		t.Fatalf("MigrateLocalToGCP: %v", err)
	}
	if report.Migrated != 1 {
		t.Fatalf("Migrated = %d, want 1", report.Migrated)
	}
	got, ok := f.smValue("API_KEY")
	if !ok {
		t.Fatal("no Secret Manager value written")
	}
	if got != plain {
		t.Fatal("Secret Manager holds a value other than the decoded original")
	}
	row, err := f.st.GetSecret(context.Background(), "API_KEY", store.ScopeUser, migScopeID)
	if err != nil {
		t.Fatalf("GetSecret: %v", err)
	}
	if row.SecretRef == "" {
		t.Fatal("migrated row has no Secret Manager reference")
	}
	assertNoLeak(t, report, nil, log, plain, raw)
}

func TestMigrateLocalToGCP_ReadBackMismatchKeepsRowAndIncomplete(t *testing.T) {
	f := newMigrationFixture(t)
	const plainA, plainB, substituted = "plain-value-a", "plain-value-b", "substituted-readback-value"
	rawA := f.seed(t, "KEY_A", plainA)
	rawB := f.seed(t, "KEY_B", plainB)
	f.sm.accessData[f.path("KEY_B", store.ScopeUser, migScopeID)] = []byte(substituted)

	report, log, err := f.run(t, false)
	if !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
	}
	if report.Migrated != 1 || report.Unverified != 1 {
		t.Fatalf("report = %+v, want 1 migrated and 1 unverified", report)
	}
	f.assertKeptLocal(t, "KEY_B", rawB, plainB)
	if got, _ := f.smValue("KEY_A"); got != plainA {
		t.Error("KEY_A was not migrated with its decoded value")
	}
	if !strings.Contains(log, "UNVERIFIED  KEY_B") {
		t.Error("log does not report KEY_B as unverified")
	}
	assertNoLeak(t, report, err, log, plainA, plainB, substituted, rawA, rawB)

	// Clearing the fault, a re-run verifies the remaining row and completes.
	f.sm.clearFaults()
	report, log, err = f.run(t, false)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if report.Migrated != 1 || report.Verified != 1 {
		t.Fatalf("re-run report = %+v, want 1 migrated and 1 verified", report)
	}
	if got, _ := f.smValue("KEY_B"); got != plainB {
		t.Error("re-run did not migrate KEY_B with its decoded value")
	}
	assertNoLeak(t, report, err, log, plainA, plainB, rawA, rawB)
}

func TestMigrateLocalToGCP_ReadBackErrorKeepsRowAndIncomplete(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-value-readback-error"
	raw := f.seed(t, "KEY_R", plain)
	f.sm.accessErr[f.path("KEY_R", store.ScopeUser, migScopeID)] = status.Error(codes.Unavailable, "backend unavailable")

	report, log, err := f.run(t, false)
	if !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
	}
	if report.Unverified != 1 || report.Migrated != 0 {
		t.Fatalf("report = %+v, want 1 unverified", report)
	}
	f.assertKeptLocal(t, "KEY_R", raw, plain)
	assertNoLeak(t, report, err, log, plain, raw)
}

func TestMigrateLocalToGCP_WriteFailureKeepsRowAndIncomplete(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-value-write-failure"
	raw := f.seed(t, "KEY_W", plain)
	f.sm.addErr[f.path("KEY_W", store.ScopeUser, migScopeID)] = status.Error(codes.Internal, "write rejected")

	report, log, err := f.run(t, false)
	if !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
	}
	if report.Failed != 1 || report.Migrated != 0 {
		t.Fatalf("report = %+v, want 1 failed", report)
	}
	if strings.Contains(log, "write rejected") {
		t.Error("log carries the Secret Manager error text")
	}
	f.assertKeptLocal(t, "KEY_W", raw, plain)
	assertNoLeak(t, report, err, log, plain, raw)
}

// undecodable is a well-formed enc:v1: envelope that no key opens.
func undecodable() string {
	return "enc:v1:" + base64.StdEncoding.EncodeToString(make([]byte, 48))
}

func TestMigrateLocalToGCP_DecodeFailureSkipsRowByID(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-value-good"
	rawGood := f.seed(t, "GOOD_KEY", plain)
	badID := tid("undecodable-row")
	bad := undecodable()
	if _, err := f.st.UpsertSecret(context.Background(), &store.Secret{
		ID: badID, Key: "BAD_KEY", EncryptedValue: bad, SecretType: store.SecretTypeEnvironment,
		Scope: store.ScopeUser, ScopeID: migScopeID,
	}); err != nil {
		t.Fatalf("UpsertSecret: %v", err)
	}

	report, log, err := f.run(t, false)
	if !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
	}
	if report.DecodeFailed != 1 || len(report.DecodeFailedIDs) != 1 || report.DecodeFailedIDs[0] != badID {
		t.Fatalf("report = %+v, want the one decode failure reported by row ID", report)
	}
	if report.Migrated != 1 {
		t.Fatalf("Migrated = %d, want 1 (the good row)", report.Migrated)
	}
	if _, ok := f.smValue("BAD_KEY"); ok {
		t.Error("an undecodable row was written to Secret Manager")
	}
	row, gErr := f.st.GetSecret(context.Background(), "BAD_KEY", store.ScopeUser, migScopeID)
	if gErr != nil {
		t.Fatalf("GetSecret: %v", gErr)
	}
	if row.SecretRef != "" {
		t.Error("an undecodable row was marked migrated")
	}
	var decodeLine string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "SKIP-DECODE") {
			decodeLine = line
		}
	}
	if !strings.Contains(decodeLine, badID) {
		t.Error("the decode-failure line does not name the row ID")
	}
	if strings.Contains(decodeLine, "BAD_KEY") || strings.Contains(decodeLine, store.ScopeUser) {
		t.Error("the decode-failure line names more than the row ID")
	}
	assertNoLeak(t, report, err, log, plain, rawGood, bad)
}

func TestMigrateLocalToGCP_DryRunDecodesAndWritesNothing(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-value-dry-run"
	raw := f.seed(t, "DRY_KEY", plain)
	badID := tid("dry-undecodable-row")
	if _, err := f.st.UpsertSecret(context.Background(), &store.Secret{
		ID: badID, Key: "DRY_BAD", EncryptedValue: undecodable(), SecretType: store.SecretTypeEnvironment,
		Scope: store.ScopeUser, ScopeID: migScopeID,
	}); err != nil {
		t.Fatalf("UpsertSecret: %v", err)
	}

	report, log, err := f.run(t, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.Migrated != 1 || report.DecodeFailed != 1 || !report.DryRun {
		t.Fatalf("report = %+v, want 1 would-migrate and 1 decode failure", report)
	}
	f.sm.mu.Lock()
	n := len(f.sm.versions)
	f.sm.mu.Unlock()
	if n != 0 {
		t.Fatalf("dry run wrote %d Secret Manager value(s)", n)
	}
	f.assertKeptLocal(t, "DRY_KEY", raw, plain)
	assertNoLeak(t, report, err, log, plain, raw)
}

func TestMigrateLocalToGCP_InternalSecretKeepsLocalBackup(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-internal-key"
	raw := f.seedAs(t, "INTERNAL_KEY", plain, store.SecretTypeInternal, store.ScopeHub, "test-hub-id")

	report, log, err := f.run(t, false)
	if err != nil {
		t.Fatalf("MigrateLocalToGCP: %v", err)
	}
	if report.Migrated != 1 {
		t.Fatalf("Migrated = %d, want 1", report.Migrated)
	}
	f.sm.mu.Lock()
	got := string(f.sm.versions[f.path("INTERNAL_KEY", store.ScopeHub, "test-hub-id")])
	f.sm.mu.Unlock()
	if got != plain {
		t.Error("Secret Manager holds a value other than the decoded original")
	}
	row, gErr := f.st.GetSecret(context.Background(), "INTERNAL_KEY", store.ScopeHub, "test-hub-id")
	if gErr != nil {
		t.Fatalf("GetSecret: %v", gErr)
	}
	if row.SecretRef == "" || row.EncryptedValue != raw {
		t.Error("an internal secret must gain a reference and keep its local backup")
	}
	assertNoLeak(t, report, err, log, plain, raw)
}

func TestMigrateLocalToGCP_RefusesWithoutCipher(t *testing.T) {
	for name, c := range map[string]*ValueCipher{"nil": nil, "zero": {}} {
		t.Run(name, func(t *testing.T) {
			f := newMigrationFixture(t)
			f.seed(t, "API_KEY", "plain-value-no-cipher")
			var log bytes.Buffer
			_, err := MigrateLocalToGCP(context.Background(), f.st, c, f.gcp, false, &log)
			if !errors.Is(err, ErrCipherNotConfigured) {
				t.Fatalf("err = %v, want ErrCipherNotConfigured", err)
			}
			f.sm.mu.Lock()
			n := len(f.sm.versions)
			f.sm.mu.Unlock()
			if n != 0 {
				t.Fatalf("wrote %d Secret Manager value(s) without a cipher", n)
			}
		})
	}
}

func TestGCPBackendSetVerified_MismatchLeavesRowUntouched(t *testing.T) {
	f := newMigrationFixture(t)
	const plain, substituted = "plain-set-verified", "other-value"
	raw := f.seed(t, "SV_KEY", plain)
	f.sm.accessData[f.path("SV_KEY", store.ScopeUser, migScopeID)] = []byte(substituted)

	_, _, err := f.gcp.SetVerified(context.Background(), &SetSecretInput{
		Name: "SV_KEY", Value: plain, SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: migScopeID,
	})
	if !errors.Is(err, ErrSecretVerify) {
		t.Fatalf("err = %v, want ErrSecretVerify", err)
	}
	if strings.Contains(err.Error(), plain) || strings.Contains(err.Error(), substituted) {
		t.Error("the verify error carries a value")
	}
	f.assertKeptLocal(t, "SV_KEY", raw, plain)
}

// seedOldRawCopy reproduces a migration made before values were decoded:
// value is stored at rest through the LocalBackend, then that stored form
// is written to Secret Manager with Set, exactly as the old migration did.
// It returns the stored form, which is now Secret Manager's value.
func (f *migrationFixture) seedOldRawCopy(t *testing.T, name, value string) string {
	t.Helper()
	raw := f.seed(t, name, value)
	if _, _, err := f.gcp.Set(context.Background(), &SetSecretInput{
		Name: name, Value: raw, SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: migScopeID,
	}); err != nil {
		t.Fatalf("old raw copy of %s: %v", name, err)
	}
	if got, _ := f.smValue(name); got != raw {
		t.Fatalf("precondition: Secret Manager must hold the at-rest form of %s", name)
	}
	return raw
}

// seedRef stores value in Secret Manager for name, the way a correct
// migration or a direct write leaves it.
func (f *migrationFixture) seedRef(t *testing.T, name, value string) {
	t.Helper()
	if _, _, err := f.gcp.Set(context.Background(), &SetSecretInput{
		Name: name, Value: value, SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: migScopeID,
	}); err != nil {
		t.Fatalf("seeding %s in Secret Manager: %v", name, err)
	}
}

func (f *migrationFixture) addCount() int {
	f.sm.fmu.Lock()
	defer f.sm.fmu.Unlock()
	return f.sm.adds
}

func TestMigrateLocalToGCP_RepairsEnvelopeFromOldRawCopy(t *testing.T) {
	for name, c := range map[string]*ValueCipher{
		"legacy":  NewLegacyValueCipher("migration-test-shared-secret"),
		"managed": ciphertestRing(t, "k1"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newMigrationFixtureWith(t, c)
			const plain = "plain-value-old-run"
			raw := f.seedOldRawCopy(t, "OLD_KEY", plain)

			report, log, err := f.run(t, false)
			if err != nil {
				t.Fatalf("MigrateLocalToGCP: %v", err)
			}
			if report.Repaired != 1 || report.Verified != 0 {
				t.Fatalf("report = %+v, want 1 repaired", report)
			}
			if got, _ := f.smValue("OLD_KEY"); got != plain {
				t.Fatal("Secret Manager does not hold the decoded original after the repair")
			}
			row, gErr := f.st.GetSecret(context.Background(), "OLD_KEY", store.ScopeUser, migScopeID)
			if gErr != nil {
				t.Fatalf("GetSecret: %v", gErr)
			}
			if row.SecretRef == "" {
				t.Fatal("repaired row lost its Secret Manager reference")
			}
			assertNoLeak(t, report, err, log, plain, raw)

			// A second run finds nothing left to repair.
			report, _, err = f.run(t, false)
			if err != nil || report.Repaired != 0 || report.Verified != 1 {
				t.Fatalf("second run: report = %+v, err = %v; want 1 verified", report, err)
			}
		})
	}
}

func TestMigrateLocalToGCP_RepairDryRunWritesNothing(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-value-old-run-dry"
	raw := f.seedOldRawCopy(t, "OLD_DRY", plain)
	before := f.addCount()

	report, log, err := f.run(t, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.Repaired != 1 {
		t.Fatalf("report = %+v, want 1 would-repair", report)
	}
	if f.addCount() != before {
		t.Fatal("dry run wrote to Secret Manager")
	}
	if got, _ := f.smValue("OLD_DRY"); got != raw {
		t.Fatal("dry run changed the Secret Manager value")
	}
	assertNoLeak(t, report, err, log, plain, raw)
}

// TestMigrateLocalToGCP_LeavesRealValuesAlone covers values that must not
// be taken for a local envelope: a plaintext value (which a legacy-mode
// cipher reads back unchanged, and a managed cipher refuses as unadmitted
// plaintext), a value that mentions the envelope prefix, and one that
// starts with it but does not parse as an envelope.
func TestMigrateLocalToGCP_LeavesRealValuesAlone(t *testing.T) {
	values := map[string]string{
		"plaintext":         "a-real-plaintext-secret-value",
		"mentions-envelope": "value-with-enc:v1:-inside",
		"does-not-parse":    "enc:v1:not-base64!",
	}
	for cname, c := range map[string]*ValueCipher{
		"legacy":  NewLegacyValueCipher("migration-test-shared-secret"),
		"managed": ciphertestRing(t, "k1"),
	} {
		for vname, value := range values {
			t.Run(cname+"/"+vname, func(t *testing.T) {
				f := newMigrationFixtureWith(t, c)
				f.seedRef(t, "REAL_KEY", value)
				before := f.addCount()

				report, log, err := f.run(t, false)
				if err != nil {
					t.Fatalf("MigrateLocalToGCP: %v", err)
				}
				if report.Verified != 1 || report.Repaired != 0 {
					t.Fatalf("report = %+v, want 1 verified and 0 repaired", report)
				}
				if f.addCount() != before {
					t.Fatal("a real value was rewritten")
				}
				if got, _ := f.smValue("REAL_KEY"); got != value {
					t.Fatal("the Secret Manager value changed")
				}
				assertNoLeak(t, report, err, log, value)
			})
		}
	}
}

func TestMigrateLocalToGCP_UnreadableRefIsIncomplete(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-value-unreadable-ref"
	f.seedRef(t, "REF_KEY", plain)
	f.sm.accessErr[f.path("REF_KEY", store.ScopeUser, migScopeID)] = status.Error(codes.PermissionDenied, "denied")

	report, log, err := f.run(t, false)
	if !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
	}
	if report.Unverified != 1 {
		t.Fatalf("report = %+v, want 1 unverified", report)
	}
	if strings.Contains(log, "denied\n") {
		t.Error("log carries the Secret Manager error text")
	}
	assertNoLeak(t, report, err, log, plain)
}

// TestMigrateLocalToGCP_UnopenableEnvelopeIsIncomplete covers a Secret
// Manager value that parses as a local envelope but does not open under
// the local key (one sealed under a key absent from the cipher, and one
// that authenticates under no key): it is not a usable value, so the run
// must stay incomplete, and the value is not rewritten.
func TestMigrateLocalToGCP_UnopenableEnvelopeIsIncomplete(t *testing.T) {
	sealed, err := NewLegacyValueCipher("a-different-shared-secret").Encrypt("plain-under-absent-key")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	for cname, c := range map[string]*ValueCipher{
		"legacy":  NewLegacyValueCipher("migration-test-shared-secret"),
		"managed": ciphertestRing(t, "k1"),
	} {
		for vname, value := range map[string]string{
			"absent-key":   sealed,
			"no-key-opens": undecodable(),
		} {
			t.Run(cname+"/"+vname, func(t *testing.T) {
				f := newMigrationFixtureWith(t, c)
				f.seedRef(t, "GONE_KEY", value)
				before := f.addCount()

				report, log, err := f.run(t, false)
				if !errors.Is(err, ErrMigrationIncomplete) {
					t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
				}
				if report.Unverified != 1 || report.Verified != 0 || report.Repaired != 0 {
					t.Fatalf("report = %+v, want 1 unverified", report)
				}
				if f.addCount() != before {
					t.Fatal("an unopenable envelope was rewritten")
				}
				if !strings.Contains(log, "UNVERIFIED  GONE_KEY") {
					t.Error("log does not report the row as unverified")
				}
				assertNoLeak(t, report, err, log, value, "plain-under-absent-key")
			})
		}
	}
}

func TestMigrateLocalToGCP_RepairReadBackMismatchKeepsPreviousVersion(t *testing.T) {
	f := newMigrationFixture(t)
	const plain, substituted = "plain-value-repair-mismatch", "substituted-repair-readback"
	raw := f.seedOldRawCopy(t, "OLD_MISMATCH", plain)

	// The first read (the check) must see the envelope; only the read-back
	// after the write is substituted.
	hook := &readBackSubstitute{faultSMClient: f.sm, path: f.path("OLD_MISMATCH", store.ScopeUser, migScopeID), value: []byte(substituted)}
	f.gcp = NewGCPBackendWithClient(f.st, hook, "test-project", "test-hub-id")

	report, log, err := f.run(t, false)
	if !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
	}
	if report.Unverified != 1 || report.Repaired != 0 {
		t.Fatalf("report = %+v, want 1 unverified", report)
	}
	if !strings.Contains(log, "previous Secret Manager version is kept one version back") {
		t.Error("the unverified repair line does not say the previous version is kept")
	}
	assertNoLeak(t, report, err, log, plain, raw, substituted)
}

// readBackSubstitute returns value for path on every read after the first.
type readBackSubstitute struct {
	*faultSMClient
	path  string
	value []byte
	reads int
}

func (r *readBackSubstitute) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	if trimVersion(req.Name) == r.path {
		r.reads++
		if r.reads > 1 {
			return &smpb.AccessSecretVersionResponse{Name: req.Name, Payload: &smpb.SecretPayload{Data: r.value}}, nil
		}
	}
	return r.faultSMClient.AccessSecretVersion(ctx, req)
}

func TestMigrateLocalToGCP_NonSecretManagerRefIsUnverified(t *testing.T) {
	f := newMigrationFixture(t)
	if _, err := f.st.UpsertSecret(context.Background(), &store.Secret{
		ID: tid("other-ref-row"), Key: "OTHER_REF", SecretRef: "other:backend/path", SecretType: store.SecretTypeEnvironment,
		Scope: store.ScopeUser, ScopeID: migScopeID,
	}); err != nil {
		t.Fatalf("UpsertSecret: %v", err)
	}

	report, log, err := f.run(t, false)
	if !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
	}
	if report.Unverified != 1 || report.Verified != 0 {
		t.Fatalf("report = %+v, want 1 unverified", report)
	}
	if !strings.Contains(log, "UNVERIFIED  OTHER_REF") {
		t.Error("log does not report the row as unverified")
	}
}

// TestMigrateLocalToGCP_KeepsInjectionModeAndAllowProgeny checks that a
// migration and a repair keep the row's injection mode and progeny
// opt-in instead of resetting them to their defaults.
func TestMigrateLocalToGCP_KeepsInjectionModeAndAllowProgeny(t *testing.T) {
	ctx := context.Background()
	input := func(name, value string) *SetSecretInput {
		return &SetSecretInput{
			Name: name, Value: value, SecretType: store.SecretTypeEnvironment, Scope: store.ScopeUser, ScopeID: migScopeID,
			InjectionMode: store.InjectionModeAlways, AllowProgeny: true,
		}
	}
	assertKept := func(t *testing.T, f *migrationFixture, name string) {
		t.Helper()
		row, err := f.st.GetSecret(ctx, name, store.ScopeUser, migScopeID)
		if err != nil {
			t.Fatalf("GetSecret %s: %v", name, err)
		}
		if row.SecretRef == "" {
			t.Fatalf("%s has no Secret Manager reference", name)
		}
		if row.InjectionMode != store.InjectionModeAlways || !row.AllowProgeny {
			t.Errorf("%s: injection mode %q, allow progeny %v; want %q and true", name, row.InjectionMode, row.AllowProgeny, store.InjectionModeAlways)
		}
	}

	t.Run("migrate", func(t *testing.T) {
		f := newMigrationFixture(t)
		if _, _, err := f.local.Set(ctx, input("FLAGS_KEY", "plain-flags-value")); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		if report, _, err := f.run(t, false); err != nil || report.Migrated != 1 {
			t.Fatalf("report = %+v, err = %v; want 1 migrated", report, err)
		}
		assertKept(t, f, "FLAGS_KEY")
	})

	t.Run("repair", func(t *testing.T) {
		f := newMigrationFixture(t)
		if _, _, err := f.local.Set(ctx, input("FLAGS_OLD", "plain-flags-old")); err != nil {
			t.Fatalf("seeding: %v", err)
		}
		raw, err := f.st.GetSecretValue(ctx, "FLAGS_OLD", store.ScopeUser, migScopeID)
		if err != nil {
			t.Fatalf("GetSecretValue: %v", err)
		}
		// The old raw copy, with the row's flags.
		if _, _, err := f.gcp.Set(ctx, input("FLAGS_OLD", raw)); err != nil {
			t.Fatalf("old raw copy: %v", err)
		}
		if report, _, err := f.run(t, false); err != nil || report.Repaired != 1 {
			t.Fatalf("report = %+v, err = %v; want 1 repaired", report, err)
		}
		assertKept(t, f, "FLAGS_OLD")
		if got, _ := f.smValue("FLAGS_OLD"); got != "plain-flags-old" {
			t.Error("the repair did not write the decoded value")
		}
	})
}

// TestMigrateLocalToGCP_RepairWriteFailureLine: when a repair's write
// fails, no Secret Manager version was added, so the line says only that
// the row is left as it was.
func TestMigrateLocalToGCP_RepairWriteFailureLine(t *testing.T) {
	f := newMigrationFixture(t)
	const plain = "plain-value-repair-write-failure"
	raw := f.seedOldRawCopy(t, "OLD_WRITE", plain)
	f.sm.addErr[f.path("OLD_WRITE", store.ScopeUser, migScopeID)] = status.Error(codes.Internal, "write rejected")

	report, log, err := f.run(t, false)
	if !errors.Is(err, ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want ErrMigrationIncomplete", err)
	}
	if report.Failed != 1 || report.Repaired != 0 {
		t.Fatalf("report = %+v, want 1 failed", report)
	}
	var line string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "ERROR  OLD_WRITE") {
			line = l
		}
	}
	if !strings.HasSuffix(line, "; the row is left as it was") {
		t.Errorf("the write-failure line must end with the row being left as it was, got %q", line)
	}
	if strings.Contains(line, "previous Secret Manager version") {
		t.Error("the write-failure line claims a kept previous version, but no version was added")
	}
	assertNoLeak(t, report, err, log, plain, raw)
}

// TestMigrateLocalToGCP_NilStoreAndLogger: a nil store is refused before
// anything is read or written, and a nil logger runs to completion with
// the log discarded.
func TestMigrateLocalToGCP_NilStoreAndLogger(t *testing.T) {
	for _, tc := range []struct {
		name    string
		nilSt   bool
		nilLog  bool
		wantErr string
	}{
		{name: "nil store", nilSt: true, wantErr: "secret migration needs a secret store"},
		{name: "nil logger", nilLog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMigrationFixture(t)
			const plain = "plain-value-nil-guards"
			f.seed(t, "NIL_KEY", plain)
			var st store.SecretStore = f.st
			if tc.nilSt {
				st = nil
			}
			var log io.Writer = &bytes.Buffer{}
			if tc.nilLog {
				log = nil
			}

			report, err := MigrateLocalToGCP(context.Background(), st, f.cipher, f.gcp, false, log)
			if tc.wantErr != "" {
				if err == nil || err.Error() != tc.wantErr {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if f.addCount() != 0 {
					t.Fatal("a refused run wrote to Secret Manager")
				}
				return
			}
			if err != nil || report.Migrated != 1 {
				t.Fatalf("report = %+v, err = %v; want 1 migrated", report, err)
			}
			if got, _ := f.smValue("NIL_KEY"); got != plain {
				t.Error("Secret Manager does not hold the decoded original")
			}
		})
	}
}
