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

package projectsync

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildWebDAVURL(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		projectID  string
		expected string
	}{
		{
			name:     "basic",
			endpoint: "https://hub.example.com",
			projectID:  "my-grove",
			expected: "https://hub.example.com/api/v1/groves/my-grove/dav",
		},
		{
			name:     "trailing slash",
			endpoint: "https://hub.example.com/",
			projectID:  "my-grove",
			expected: "https://hub.example.com/api/v1/groves/my-grove/dav",
		},
		{
			name:     "with port",
			endpoint: "http://localhost:8080",
			projectID:  "test-grove-123",
			expected: "http://localhost:8080/api/v1/groves/test-grove-123/dav",
		},
		{
			name:     "uuid grove id",
			endpoint: "https://hub.example.com",
			projectID:  "550e8400-e29b-41d4-a716-446655440000",
			expected: "https://hub.example.com/api/v1/groves/550e8400-e29b-41d4-a716-446655440000/dav",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildWebDAVURL(tt.endpoint, tt.projectID)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestSync_ValidationErrors(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name   string
		opts   Options
		errMsg string
	}{
		{
			name:   "missing local path",
			opts:   Options{HubEndpoint: "https://hub.example.com", ProjectID: "test"},
			errMsg: "local workspace path is required",
		},
		{
			name:   "missing hub endpoint",
			opts:   Options{LocalPath: "/tmp/workspace", ProjectID: "test"},
			errMsg: "hub endpoint is required",
		},
		{
			name:   "missing grove ID",
			opts:   Options{LocalPath: "/tmp/workspace", HubEndpoint: "https://hub.example.com"},
			errMsg: "grove ID is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Sync(ctx, tt.opts)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestDefaultExcludePatterns(t *testing.T) {
	// Verify the default exclude patterns match what the hub WebDAV endpoint excludes
	expected := []string{
		".git/**",
		".scion/**",
		"node_modules/**",
		"*.env",
	}
	assert.Equal(t, expected, DefaultExcludePatterns)
}

func TestDirection_Values(t *testing.T) {
	assert.Equal(t, Direction("push"), DirPush)
	assert.Equal(t, Direction("pull"), DirPull)
	assert.Equal(t, Direction("bisync"), DirBisync)
}

func TestBuildRcloneRemoteString(t *testing.T) {
	// Verify that the remote string properly single-quotes values so that
	// special characters (e.g. "://" in URLs) are not parsed as rclone
	// connection-string delimiters.
	davURL := buildWebDAVURL("https://hub.example.com", "grove-123")
	token := "eyJhbGciOiJIUzI1NiJ9.test.signature"

	remote := fmt.Sprintf(":webdav,url='%s',bearer_token='%s':", davURL, token)

	assert.Contains(t, remote, "url='https://hub.example.com/api/v1/groves/grove-123/dav'")
	assert.Contains(t, remote, "bearer_token='eyJhbGciOiJIUzI1NiJ9.test.signature'")
	assert.True(t, strings.HasPrefix(remote, ":webdav,"))
	assert.True(t, strings.HasSuffix(remote, ":"))
}
