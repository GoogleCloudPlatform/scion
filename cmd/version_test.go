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
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type versionRoundTripper func(*http.Request) (*http.Response, error)

func (f versionRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func runVersionCommand() error {
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	return versionCmd.RunE(cmd, nil)
}

func setVersionTestState(t *testing.T, currentVersion, format string) {
	t.Helper()

	originalVersion := version.Version
	originalCommit := version.Commit
	originalBuildTime := version.BuildTime
	originalFormat := outputFormat
	originalCheckUpdate := checkUpdate
	originalHTTPClient := http.DefaultClient
	t.Cleanup(func() {
		version.Version = originalVersion
		version.Commit = originalCommit
		version.BuildTime = originalBuildTime
		outputFormat = originalFormat
		checkUpdate = originalCheckUpdate
		http.DefaultClient = originalHTTPClient
	})

	version.Version = currentVersion
	version.Commit = "abc123456789"
	version.BuildTime = "2026-09-15T00:00:00Z"
	outputFormat = format
	checkUpdate = true
	t.Setenv("SCION_CLI_MODE", "agent")
}

func TestVersionCheckFlag(t *testing.T) {
	flag := versionCmd.Flags().Lookup("check")
	require.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)
	assert.Equal(t, "Check for available updates", flag.Usage)
}

func TestVersionCheckJSONIncludesUpdateInfo(t *testing.T) {
	setVersionTestState(t, "v0.3.0-preview.2", "json")
	http.DefaultClient = &http.Client{Transport: versionRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(strings.NewReader(`{
				"channels": {
					"preview": {
						"version": "v0.3.0-preview.3",
						"url": "https://example.com/v0.3.0-preview.3"
					}
				}
			}`)),
		}, nil
	})}

	output := captureStdout(t, func() {
		require.NoError(t, runVersionCommand())
	})

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	assert.Equal(t, "preview", result["channel"])
	assert.Equal(t, true, result["updateAvailable"])
	assert.Equal(t, "v0.3.0-preview.3", result["latestVersion"])
	assert.Equal(t, "https://example.com/v0.3.0-preview.3", result["releaseUrl"])
}

func TestVersionCheckTextReportsAvailableUpdate(t *testing.T) {
	setVersionTestState(t, "v0.3.0-preview.2", "")
	http.DefaultClient = &http.Client{Transport: versionRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"channels":{"preview":{"version":"v0.3.0-preview.3","url":"https://example.com/release"}}}`)),
		}, nil
	})}

	output := captureStdout(t, func() {
		require.NoError(t, runVersionCommand())
	})

	assert.Equal(t, "scion v0.3.0-preview.2 (commit abc12345)\n\nUpdate available: v0.3.0-preview.3\n  https://example.com/release\n", output)
}

func TestVersionCheckTextReportsLatestVersion(t *testing.T) {
	setVersionTestState(t, "v0.3.0-preview.3", "")
	http.DefaultClient = &http.Client{Transport: versionRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body:       io.NopCloser(strings.NewReader(`{"channels":{"preview":{"version":"v0.3.0-preview.3"}}}`)),
		}, nil
	})}

	output := captureStdout(t, func() {
		require.NoError(t, runVersionCommand())
	})

	assert.Equal(t, "scion v0.3.0-preview.3 (commit abc12345)\n\nYou are running the latest preview version.\n", output)
}

func TestVersionCheckTextReportsErrorToStderr(t *testing.T) {
	setVersionTestState(t, "v1.0.0", "")
	http.DefaultClient = &http.Client{Transport: versionRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}

	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			require.NoError(t, runVersionCommand())
		})
	})

	assert.Equal(t, "scion v1.0.0 (commit abc12345)\n", stdout)
	assert.Contains(t, stderr, "Could not check for updates: check for update: fetch release manifest")
	assert.Contains(t, stderr, "network unavailable")
}

func TestVersionCheckJSONReportsError(t *testing.T) {
	setVersionTestState(t, "v1.0.0", "json")
	http.DefaultClient = &http.Client{Transport: versionRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}

	output := captureStdout(t, func() {
		require.NoError(t, runVersionCommand())
	})

	var result map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(output), &result))
	assert.Contains(t, result["updateError"], "network unavailable")
	assert.NotContains(t, result, "updateAvailable")
}

func TestVersionCheckDevBuildSkipsUpdateOutput(t *testing.T) {
	setVersionTestState(t, "", "")
	http.DefaultClient = &http.Client{Transport: versionRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("development build should not fetch the release manifest")
		return nil, nil
	})}

	output := captureStdout(t, func() {
		require.NoError(t, runVersionCommand())
	})

	assert.Equal(t, "scion dev (commit abc12345)\n\nUpdate checking is not supported for development or unknown builds.\n", output)
}
