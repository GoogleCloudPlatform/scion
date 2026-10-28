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

//go:build windows

package cmd

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
)

// Broker join is gated on Windows, but its token-file
// warning reads the real DACL (privatefs.CheckPrivate), not mode bits: a
// file granting Users read warns, naming the SID, and is still used.
// Control: the same file with an owner-only DACL does not warn.
func TestResolveBrokerJoinToken_WarnsOnForeignACE(t *testing.T) {
	p := filepath.Join(t.TempDir(), "join-token")
	require.NoError(t, os.WriteFile(p, []byte("scion_join_x\n"), 0o600))
	u := wintest.UserSID(t)
	wintest.SetDACL(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FR;;;%s)", u, wintest.SIDUsers))
	var errOut bytes.Buffer
	got, err := resolveBrokerJoinToken(p, nil, &errOut)
	require.NoError(t, err)
	assert.Equal(t, "scion_join_x", got)
	assert.Contains(t, errOut.String(), "has group or other permissions")
	assert.Contains(t, errOut.String(), wintest.SIDUsers)
	assert.Contains(t, errOut.String(), "owner-only ACL")
	assert.NotContains(t, errOut.String(), "scion_join_x")

	wintest.SetDACL(t, p, fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;%s)", u, wintest.SIDSystem))
	errOut.Reset()
	_, err = resolveBrokerJoinToken(p, nil, &errOut)
	require.NoError(t, err)
	assert.Empty(t, errOut.String(), "control: a private file does not warn")
}
