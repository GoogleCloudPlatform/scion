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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubUploadsConfig_Defaults: an unset block keeps the limits the hub had
// before they were configurable.
func TestHubUploadsConfig_Defaults(t *testing.T) {
	var c HubUploadsConfig
	assert.True(t, c.IsZero())
	assert.NoError(t, c.Validate())
	assert.Equal(t, int64(10<<20), c.EffectiveMaxAttachmentSize())
	assert.Equal(t, int64(50<<20), c.EffectiveMaxWorkspaceFileSize())

	big := HubUploadsConfig{MaxAttachmentSize: 100 << 20, MaxWorkspaceFileSize: 100 << 20}
	assert.NoError(t, big.Validate(), "100 MiB per file is accepted")
	assert.Equal(t, int64(100<<20), big.EffectiveMaxAttachmentSize())
	assert.Equal(t, int64(100<<20), big.EffectiveMaxWorkspaceFileSize())
}

// TestHubUploadsConfig_Validate refuses negative values and per-file limits
// above their ceilings (MaxUploadFileSize for attachments,
// MaxLargeUploadFileSize for workspace files).
func TestHubUploadsConfig_Validate(t *testing.T) {
	for _, ok := range []HubUploadsConfig{
		{MaxAttachmentSize: 1},
		{MaxWorkspaceFileSize: MaxUploadFileSize},
		{MaxWorkspaceFileSize: MaxLargeUploadFileSize},
		{MaxAttachmentSize: MaxUploadFileSize},
	} {
		assert.NoError(t, ok.Validate(), "%+v", ok)
	}
	for _, bad := range []HubUploadsConfig{
		{MaxAttachmentSize: -1},
		{MaxWorkspaceFileSize: -1},
		{MaxAttachmentSize: MaxUploadFileSize + 1},
		{MaxWorkspaceFileSize: MaxLargeUploadFileSize + 1},
	} {
		assert.Error(t, bad.Validate(), "%+v", bad)
	}
}

// TestHubUploadsSettings_RoundTripAndEnv: V1 to GlobalConfig and back, an
// unset block omitted, and the schema's env names reach the loaded config.
func TestHubUploadsSettings_RoundTripAndEnv(t *testing.T) {
	v1 := &V1ServerConfig{Hub: &V1ServerHubConfig{Uploads: &V1ServerHubUploadsConfig{
		MaxAttachmentSize: 32 << 20, MaxWorkspaceFileSize: 80 << 20,
	}}}
	gc := ConvertV1ServerToGlobalConfig(v1)
	assert.Equal(t, HubUploadsConfig{
		MaxAttachmentSize: 32 << 20, MaxWorkspaceFileSize: 80 << 20,
	}, gc.Hub.Uploads)

	back := ConvertGlobalToV1ServerConfig(gc)
	require.NotNil(t, back.Hub)
	require.NotNil(t, back.Hub.Uploads)
	assert.Equal(t, *v1.Hub.Uploads, *back.Hub.Uploads)
	assert.Nil(t, ConvertGlobalToV1ServerConfig(&GlobalConfig{}).Hub.Uploads, "unset limits are omitted")

	t.Setenv("HOME", t.TempDir())
	t.Setenv("SCION_SERVER_HUB_UPLOADS_MAXATTACHMENTSIZE", "33554432")
	t.Setenv("SCION_SERVER_HUB_UPLOADS_MAXWORKSPACEFILESIZE", "83886080")
	cfg, err := LoadGlobalConfig("")
	require.NoError(t, err)
	assert.Equal(t, int64(32<<20), cfg.Hub.Uploads.MaxAttachmentSize)
	assert.Equal(t, int64(80<<20), cfg.Hub.Uploads.MaxWorkspaceFileSize)
}
