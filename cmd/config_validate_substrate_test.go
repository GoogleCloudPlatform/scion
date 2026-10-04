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
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGlobalSettingsForValidate points HOME at a fresh temp dir, chdirs into
// it (so project-root discovery can't wander up into a real checkout's
// .scion directory), and writes data as the global settings.yaml.
func writeGlobalSettingsForValidate(t *testing.T, data string) {
	t.Helper()

	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Chdir(tmpHome)

	oldProjectPath := projectPath
	projectPath = ""
	t.Cleanup(func() { projectPath = oldProjectPath })

	oldFormat := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = oldFormat })

	globalDir, err := config.GetGlobalDir()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(globalDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(data), 0644))
}

// runConfigValidate invokes configValidateCmd.RunE and captures its stdout.
// The command's returned error is a generic "validation failed" sentinel;
// the per-file, per-field cause is only in the printed "ERROR: ..." lines,
// so tests that need to pin the cause must inspect the captured output.
func runConfigValidate(t *testing.T) (output string, err error) {
	t.Helper()

	oldStdout := os.Stdout
	r, w, pipeErr := os.Pipe()
	require.NoError(t, pipeErr)
	os.Stdout = w

	cmd := configValidateCmd
	err = cmd.RunE(cmd, nil)

	require.NoError(t, w.Close())
	os.Stdout = oldStdout

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)
	return buf.String(), err
}

func TestConfigValidateCmd_SubstrateEgressTrustBundleValid(t *testing.T) {
	writeGlobalSettingsForValidate(t, `
schema_version: "1"
runtimes:
  substrate-prod:
    type: substrate
    substrate:
      api_endpoint: "api.ate-system.svc:443"
      router_endpoint: "http://atenet-router.ate-system.svc:80"
      egress_trust_bundle: egress-mitm.ate.dev
`)

	cmd := configValidateCmd
	err := cmd.RunE(cmd, nil)
	require.NoError(t, err, "a documented egress_trust_bundle value should validate cleanly via `scion config validate`")
}

func TestConfigValidateCmd_SubstrateBogusKeyFails(t *testing.T) {
	writeGlobalSettingsForValidate(t, `
schema_version: "1"
runtimes:
  substrate-prod:
    type: substrate
    substrate:
      api_endpoint: "api.ate-system.svc:443"
      router_endpoint: "http://atenet-router.ate-system.svc:80"
      bogus_field: "nope"
`)

	output, err := runConfigValidate(t)
	require.Error(t, err, "an unknown key in the substrate object should fail `scion config validate`")
	assert.Contains(t, output, "runtimes/substrate-prod/substrate",
		"the error output should name the substrate object path that failed validation")
}

func TestConfigValidateCmd_SubstrateEgressTrustBundleInvalidValue(t *testing.T) {
	writeGlobalSettingsForValidate(t, `
schema_version: "1"
runtimes:
  substrate-prod:
    type: substrate
    substrate:
      api_endpoint: "api.ate-system.svc:443"
      router_endpoint: "http://atenet-router.ate-system.svc:80"
      egress_trust_bundle: some-other-bundle
`)

	output, err := runConfigValidate(t)
	require.Error(t, err, "an unsupported egress_trust_bundle value should fail `scion config validate`")
	assert.Contains(t, output, "runtimes/substrate-prod/substrate/egress_trust_bundle",
		"the error output should name the egress_trust_bundle field that failed validation")
}
