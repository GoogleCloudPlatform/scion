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

package secret

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	st, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	c := NewLegacyValueCipher("migration-test-shared-secret")
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
	if report.Migrated != 1 || report.AlreadyRef != 1 {
		t.Fatalf("re-run report = %+v, want 1 migrated and 1 already migrated", report)
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
