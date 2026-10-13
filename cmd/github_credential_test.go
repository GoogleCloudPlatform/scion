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
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	credTestRepoKey  = "GH_ACME_CORP__PRIVATE_CONFIGS"
	credTestOwnerKey = "GH_ACME_CORP"
	credTestSource   = "https://github.com/acme-corp/private.configs/tree/main/harness-configs/custom"
)

// clearGitHubCredentialEnv unsets the variables remoteFetchCredential reads,
// so a credential leaked from the container cannot decide a test.
func clearGitHubCredentialEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{credTestRepoKey, credTestOwnerKey, "GITHUB_TOKEN", "GH_OTHER_ORG"} {
		t.Setenv(name, "")
	}
}

// TestRemoteFetchCredential verifies the CLI cascade: GH_{OWNER}__{REPO}
// beats GH_{OWNER}, which beats GITHUB_TOKEN; with none set the fetch is
// unauthenticated; and the debug log names the variable, never its value.
func TestRemoteFetchCredential(t *testing.T) {
	t.Cleanup(func() { loglevel.Reset(false) })
	loglevel.Apply(loglevel.Spec{Default: slog.LevelDebug}, loglevel.SourceFlag)

	tests := []struct {
		name     string
		env      map[string]string
		want     string
		wantName string
	}{
		{"repo key beats owner and default", map[string]string{credTestRepoKey: "fake-repo-cred", credTestOwnerKey: "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"}, "fake-repo-cred", credTestRepoKey},
		{"owner key beats default", map[string]string{credTestOwnerKey: "fake-owner-cred", "GITHUB_TOKEN": "fake-default-cred"}, "fake-owner-cred", credTestOwnerKey},
		{"default when no convention key", map[string]string{"GITHUB_TOKEN": "fake-default-cred"}, "fake-default-cred", "GITHUB_TOKEN"},
		{"keys for another owner are ignored", map[string]string{"GH_OTHER_ORG": "fake-other-cred"}, "", ""},
		{"unauthenticated when nothing is set", map[string]string{}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clearGitHubCredentialEnv(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			var got string
			stderr := captureStderr(t, func() { got = remoteFetchCredential(credTestSource) })
			assert.Equal(t, tt.want, got)
			if tt.wantName != "" && util.DebugEnabled() {
				assert.Contains(t, stderr, "using credential "+tt.wantName)
			}
			for k, v := range tt.env {
				assert.NotContains(t, stderr, v, "log contains the value of %s", k)
			}
		})
	}
}

// TestRemoteFetchCredential_NonGitHubReadsNothing verifies that sources
// which are not GitHub folders get no credential even when all are set.
func TestRemoteFetchCredential_NonGitHubReadsNothing(t *testing.T) {
	clearGitHubCredentialEnv(t)
	t.Setenv("GH_ACME_CORP__CONFIGS", "fake-repo-cred")
	t.Setenv(credTestOwnerKey, "fake-owner-cred")
	t.Setenv("GITHUB_TOKEN", "fake-default-cred")
	for _, source := range []string{
		"https://github.com/acme-corp/configs/archive/main.tar.gz",
		"https://example.com/acme-corp/configs.tgz",
		":gcs:bucket/acme-corp/configs",
	} {
		assert.Empty(t, remoteFetchCredential(source), source)
	}
}

// TestResolveInstallSource_SendsGitHubCredential verifies that
// 'harness-config install' passes the CLI credential to the fetch. The
// tarball request is answered 404, so the fetch falls back to the
// sparse-checkout seam, which records the credential it was given and writes
// a config.yaml; no real git runs.
func TestResolveInstallSource_SendsGitHubCredential(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	clearGitHubCredentialEnv(t)
	t.Setenv(credTestOwnerKey, "fake-owner-cred")
	t.Setenv("GITHUB_TOKEN", "fake-default-cred")

	t.Cleanup(config.SetGitLsRemoteForTest(func(context.Context, string) ([]byte, error) {
		return nil, errors.New("ls-remote disabled in test")
	}))
	var tarballAuth string
	oldTransport := http.DefaultClient.Transport
	t.Cleanup(func() { http.DefaultClient.Transport = oldTransport })
	http.DefaultClient.Transport = credTestRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if a := r.Header.Get("Authorization"); a != "" {
			tarballAuth = a
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	var checkoutToken string
	t.Cleanup(config.SetGitSparseCheckoutForTest(func(_ context.Context, _ *config.GitHubURLParts, destPath, token string) error {
		checkoutToken = token
		return os.WriteFile(filepath.Join(destPath, "config.yaml"), []byte("harness: claude\n"), 0o644)
	}))

	localPath, cleanup, err := resolveInstallSource(credTestSource)
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	defer cleanup()
	assert.FileExists(t, filepath.Join(localPath, "config.yaml"))
	assert.Equal(t, "fake-owner-cred", checkoutToken)
	if tarballAuth != "" {
		assert.Equal(t, "Bearer fake-owner-cred", tarballAuth)
	}
}

type credTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (f credTestRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
