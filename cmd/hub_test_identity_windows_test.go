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

// Windows tests of the hub test-identity --out token file: it is created
// exclusively with the private descriptor, and the output says truthfully
// how it is protected.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/internal/testutil/wintest"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// The token file is private (owner-only ACL, set at creation, although
// the directory grants Users read inheritably) and the output line reads
// "(owner-only ACL)", never "(mode 0600)". Refusals with controls: an
// existing file, a symlinked leaf and a device-namespace path are refused;
// a new plain name in the same directory is accepted.
func TestHubTestIdentityOut_DescribesACL(t *testing.T) {
	dir := t.TempDir()
	wintest.Icacls(t, dir, "/grant", "*"+wintest.SIDUsers+":(OI)(CI)R")
	out := filepath.Join(dir, "token.txt")
	f, err := createTokenFile(out)
	require.NoError(t, err)
	require.NoError(t, f.write("FIXTURE-TOKEN"))
	assertPrivateSD(t, out)
	assert.Equal(t, "FIXTURE-TOKEN\n", readTestFile(t, out))

	var buf bytes.Buffer
	require.NoError(t, printTestIdentityToken(&buf, "Issued", &hubclient.TestIdentityTokenResponse{}, out, false))
	assert.Contains(t, buf.String(), "  Token file:    "+out+" (owner-only ACL)\n")
	assert.NotContains(t, buf.String(), "mode 0600")

	// Refused: the file now exists.
	_, err = createTokenFile(out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
	// Refused: a symlink at the leaf, even a dangling one.
	link := filepath.Join(dir, "link.txt")
	wintest.MustSymlink(t, filepath.Join(dir, "nowhere"), link)
	_, err = createTokenFile(link)
	require.Error(t, err)
	assert.NoFileExists(t, filepath.Join(dir, "nowhere"))
	// Refused: a device-namespace path (pathcanon.ValidInputPath).
	_, err = createTokenFile(`\\.\pipe\scion-token`)
	require.Error(t, err)

	// Control: a new name in the same directory; abandon removes it.
	f2, err := createTokenFile(filepath.Join(dir, "second.txt"))
	require.NoError(t, err, "control: a new plain name")
	f2.abandon()
	_, err = os.Lstat(filepath.Join(dir, "second.txt"))
	assert.True(t, os.IsNotExist(err), "abandon removed the empty file")
}
