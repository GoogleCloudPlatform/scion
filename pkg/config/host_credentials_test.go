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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostCredentialsEligible(t *testing.T) {
	assert.True(t, HostCredentialsEligible(false, true), "workstation with dev auth")
	assert.False(t, HostCredentialsEligible(false, false), "workstation without dev auth")
	assert.False(t, HostCredentialsEligible(true, true), "hosted with dev auth")
	assert.False(t, HostCredentialsEligible(true, false), "hosted without dev auth")
}

func TestUseHostCredentials(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"), []byte(content), 0o644))
	}

	// No settings file: workstation default.
	assert.True(t, UseHostCredentials(dir))

	write("schema_version: \"1\"\n")
	assert.True(t, UseHostCredentials(dir), "key unset")

	write("schema_version: \"1\"\nuse_host_credentials: false\n")
	assert.False(t, UseHostCredentials(dir))

	write("schema_version: \"1\"\nuse_host_credentials: true\n")
	assert.True(t, UseHostCredentials(dir))

	// Fails closed when the file cannot be parsed.
	write("schema_version: \"1\"\nuse_host_credentials: [unterminated\n")
	assert.False(t, UseHostCredentials(dir), "malformed settings must fail closed")

	assert.False(t, UseHostCredentials(""), "no global dir must fail closed")
}

// TestUseHostCredentials_ConfigSet checks the documented off switch:
// `scion config set use_host_credentials false` writes the key and the
// broker's per-start read sees it.
func TestUseHostCredentials_ConfigSet(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, UpdateVersionedSetting(dir, "use_host_credentials", "false"))
	assert.False(t, UseHostCredentials(dir))
	vs, err := LoadSingleFileVersioned(dir)
	require.NoError(t, err)
	v, err := GetVersionedSettingValue(vs, "use_host_credentials")
	require.NoError(t, err)
	assert.Equal(t, "false", v)

	require.NoError(t, UpdateVersionedSetting(dir, "use_host_credentials", "true"))
	assert.True(t, UseHostCredentials(dir))
}

func TestValidateSettings_UseHostCredentials(t *testing.T) {
	errs, err := ValidateSettings([]byte("schema_version: \"1\"\nuse_host_credentials: false\n"), "1")
	require.NoError(t, err)
	assert.Empty(t, errs)

	errs, err = ValidateSettings([]byte("schema_version: \"1\"\nuse_host_credentials: \"yes\"\n"), "1")
	require.NoError(t, err)
	require.NotEmpty(t, errs)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Path+" "+e.Message, "use_host_credentials") {
			found = true
		}
	}
	assert.True(t, found, "expected an error naming use_host_credentials, got: %v", errs)
}
