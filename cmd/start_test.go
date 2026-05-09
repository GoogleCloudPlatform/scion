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

func TestDeprecatedNotifyFlag(t *testing.T) {
	f := startCmd.Flags().Lookup("notify")
	require.NotNil(t, f, "--notify flag should be registered for back-compat")
	assert.NotEmpty(t, f.Deprecated, "--notify should be marked as deprecated")
	assert.Contains(t, f.Deprecated, "default")
}

func TestNoNotifyFlagRegistered(t *testing.T) {
	f := startCmd.Flags().Lookup("no-notify")
	require.NotNil(t, f, "--no-notify flag should be registered")
	assert.Empty(t, f.Deprecated, "--no-notify should not be deprecated")
	assert.Equal(t, "false", f.DefValue)
}
