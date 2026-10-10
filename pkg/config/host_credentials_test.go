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

func TestHostCredentialsPolicy(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name    string
		hosted  bool
		devAuth bool
		use     *bool
		want    bool
	}{
		{"workstation dev auth unset defaults on", false, true, nil, true},
		{"workstation dev auth explicit true", false, true, &on, true},
		{"workstation dev auth explicit false", false, true, &off, false},
		{"workstation without dev auth", false, false, nil, false},
		{"workstation without dev auth explicit true", false, false, &on, false},
		{"hosted ignores unset", true, true, nil, false},
		{"hosted ignores explicit true", true, true, &on, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, HostCredentialsPolicy(tt.hosted, tt.devAuth, tt.use))
		})
	}
}

func TestHostCredentialsEnabled_ReadsSetting(t *testing.T) {
	dir := t.TempDir()
	// No settings file: workstation default.
	assert.True(t, HostCredentialsEnabled(dir, false, true))
	assert.False(t, HostCredentialsEnabled(dir, true, true))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nuse_host_credentials: false\n"), 0o644))
	assert.False(t, HostCredentialsEnabled(dir, false, true))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "settings.yaml"),
		[]byte("schema_version: \"1\"\nuse_host_credentials: true\n"), 0o644))
	assert.True(t, HostCredentialsEnabled(dir, false, true))
	assert.False(t, HostCredentialsEnabled(dir, false, false))
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
