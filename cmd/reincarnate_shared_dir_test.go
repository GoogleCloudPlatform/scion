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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSharedDirBackendFlags(t *testing.T) {
	got, err := parseSharedDirBackendFlags([]string{"notes=nfs", "build-cache=nfs"}, true)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"notes": "nfs", "build-cache": "nfs"}, got)

	got, err = parseSharedDirBackendFlags(nil, false)
	require.NoError(t, err)
	assert.Nil(t, got)

	for _, tc := range []struct {
		values     []string
		allowEmpty bool
		want       string
	}{
		{nil, true, "--allow-empty-shared-dir needs --shared-dir-backend"},
		{[]string{"notes"}, false, "want NAME=nfs"},
		{[]string{"=nfs"}, false, "want NAME=nfs"},
		{[]string{"notes=local"}, false, "only nfs is supported"},
		{[]string{"Notes=nfs"}, false, "invalid shared dir name"},
		{[]string{"notes=nfs", "notes=nfs"}, false, "more than once"},
	} {
		_, err := parseSharedDirBackendFlags(tc.values, tc.allowEmpty)
		require.Error(t, err, "%v", tc.values)
		assert.Contains(t, err.Error(), tc.want)
	}
}

func TestReincarnateCmd_SharedDirFlagsRegistered(t *testing.T) {
	f := reincarnateCmd.Flags().Lookup("shared-dir-backend")
	require.NotNil(t, f)
	assert.Equal(t, "stringArray", f.Value.Type())
	require.NotNil(t, reincarnateCmd.Flags().Lookup("allow-empty-shared-dir"))
}
