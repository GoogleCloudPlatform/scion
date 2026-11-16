//go:build unix && !no_sqlite

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
	"os"
	"path/filepath"
	"testing"

	"cloud.google.com/go/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	scionruntime "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestFlatInstanceActivation_CloudRunRuntimePassesAllAgentSAs(t *testing.T) {
	dir := t.TempDir()
	keyPath := crWriteFile(t, dir, "broker-a.json", crSAKeyJSON(t, crPrincipalA), 0o600)
	inst := crInst("cr-east", "us-central1", keyPath)
	inst.RuntimeTarget.AssignableServiceAccounts = []string{crAssignSA}

	var gotCfg config.CloudRunConfig
	a := newFlatInstanceActivation(crHostFacts())
	a.hubPrincipal = func(context.Context) (string, error) { return "hub@example-project.iam.gserviceaccount.com", nil }
	a.newCloudRunRuntime = func(cfg config.CloudRunConfig, creds *auth.Credentials) (scionruntime.Runtime, error) {
		gotCfg = cfg
		require.NotNil(t, creds)
		return newCRFakeRuntime(cfg, nil, nil), nil
	}

	require.NoError(t, a.precondition(context.Background(), inst))
	_, err := a.newRuntime(context.Background(), inst)
	require.NoError(t, err)

	assert.Equal(t, crAgentSA, gotCfg.ServiceAccount)
	assert.Equal(t, crBlockSA, gotCfg.BlockServiceAccount)
	assert.Equal(t, []string{crAssignSA}, gotCfg.AssignableServiceAccounts)

	// Mutating the instance target slice after construction must not mutate gotCfg.
	inst.RuntimeTarget.AssignableServiceAccounts[0] = "mutated@example-project.iam.gserviceaccount.com"
	assert.Equal(t, []string{crAssignSA}, gotCfg.AssignableServiceAccounts)
}

func TestDoctorFlatCloudRun_UnixCredentialsChecksNeverLeakPathOrSecret(t *testing.T) {
	dir := t.TempDir()
	keyBytes := crSAKeyJSON(t, crPrincipalA)
	goodPath := crWriteFile(t, dir, "good-creds.json", keyBytes, 0o600)
	badModePath := crWriteFile(t, dir, "world-readable-creds.json", keyBytes, 0o644)
	execModePath := crWriteFile(t, dir, "exec-owner-creds.json", keyBytes, 0o700)
	symlinkPath := filepath.Join(dir, "symlink-creds.json")
	require.NoError(t, os.Symlink(goodPath, symlinkPath))
	sameAsBlockBytes := crSAKeyJSON(t, crBlockSA)
	sameAsBlockPath := crWriteFile(t, dir, "block-principal-creds.json", sameAsBlockBytes, 0o600)

	t.Run("valid 0600 credentials file passes P6-P9 and builds real *auth.Credentials", func(t *testing.T) {
		inst := crInst("cr-east", "us-central1", goodPath)
		inst.RuntimeTarget.AssignableServiceAccounts = []string{crAssignSA}
		stubDiag := &stubDiagnosable{
			report: scionruntime.DiagnosticReport{
				Runtime: "cloudrun",
				Checks:  []scionruntime.CheckResult{{Name: "Cloud Run Instance Service Account (actAs)", Status: "pass", Message: "ok"}},
			},
		}
		deps := defaultFlatCloudRunDoctorDeps()
		var builtCreds *auth.Credentials
		deps.newRuntime = func(cfg config.CloudRunConfig, creds *auth.Credentials) (scionruntime.Diagnosable, error) {
			builtCreds = creds
			assert.Equal(t, crAgentSA, cfg.ServiceAccount)
			assert.Equal(t, crBlockSA, cfg.BlockServiceAccount)
			assert.Equal(t, []string{crAssignSA}, cfg.AssignableServiceAccounts)
			return stubDiag, nil
		}

		got := checkDoctorFlatCloudRunInstances([]config.V1RuntimeBrokerInstanceConfig{inst}, nil, deps)
		require.Len(t, got, 1)
		assert.NotNil(t, builtCreds)
		assert.Equal(t, "pass", got[0].Status)
		assert.Equal(t, "flat-cloudrun[cr-east]: Cloud Run Instance Service Account (actAs)", got[0].Name)
	})

	for _, tc := range []struct {
		name    string
		path    string
		wantSub string
	}{
		{name: "0644 mode rejected", path: badModePath, wantSub: "(0600 or 0400)"},
		{name: "0700 mode rejected", path: execModePath, wantSub: "mode 0600 or 0400"},
		{name: "symlink rejected", path: symlinkPath, wantSub: "must not be a symlink"},
		{name: "principal equal to block_service_account rejected", path: sameAsBlockPath, wantSub: "block_service_account must differ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := crInst("cr-east", "us-central1", tc.path)
			got := checkDoctorFlatCloudRunInstances([]config.V1RuntimeBrokerInstanceConfig{inst}, nil, defaultFlatCloudRunDoctorDeps())
			require.Len(t, got, 1)
			assert.Equal(t, "fail", got[0].Status)
			assert.Equal(t, "flat-cloudrun[cr-east]: credentials", got[0].Name)
			assert.Contains(t, got[0].Message, tc.wantSub)
			for _, field := range []string{got[0].Name, got[0].Message, got[0].Remediation} {
				assert.NotContains(t, field, tc.path, "must never echo credentials_file path")
				assert.NotContains(t, field, "BEGIN PRIVATE KEY", "must never echo key contents")
			}
		})
	}
}
