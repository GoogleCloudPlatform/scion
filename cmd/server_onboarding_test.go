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
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// quickstartProbeRecorder records what printWorkstationQuickstart asked of
// its stubbed seams.
type quickstartProbeRecorder struct {
	statusCalls int
	statusToken string
	statusURL   string
	opened      []string
	signInCalls int
}

// stubQuickstartProbes replaces the readiness wait, the onboarding status
// call and the browser seams so printWorkstationQuickstart never waits on,
// talks to, or opens a real server. The browser rule defaults to "not
// allowed"; tests that check opening set quickstartBrowserAllowed.
func stubQuickstartProbes(t *testing.T, ready bool, status onboardingStatusResult, statusErr error) *quickstartProbeRecorder {
	t.Helper()
	// printWorkstationQuickstart registers the local hub in
	// ~/.scion/context.yaml: keep it out of the real HOME.
	t.Setenv("HOME", t.TempDir())
	rec := &quickstartProbeRecorder{}
	origWait, origTimeout, origStatus := quickstartWaitReady, quickstartReadyTimeout, quickstartOnboardingStatus
	origAllowed, origOpen, origSignIn, origTTY := quickstartBrowserAllowed, quickstartOpenBrowser, quickstartDevSignIn, quickstartStdoutIsTerminal
	t.Cleanup(func() {
		quickstartWaitReady, quickstartReadyTimeout, quickstartOnboardingStatus = origWait, origTimeout, origStatus
		quickstartBrowserAllowed, quickstartOpenBrowser, quickstartDevSignIn, quickstartStdoutIsTerminal = origAllowed, origOpen, origSignIn, origTTY
	})
	// stdout counts as a terminal unless a test says otherwise.
	quickstartStdoutIsTerminal = func() bool { return true }
	quickstartReadyTimeout = time.Millisecond
	quickstartWaitReady = func(string, int, time.Duration) (bool, healthProbeResponse) {
		if ready {
			return true, healthProbeResponse{Status: probeStatusHealthy}
		}
		return false, healthProbeResponse{}
	}
	quickstartOnboardingStatus = func(baseURL, token string) (onboardingStatusResult, error) {
		rec.statusCalls++
		rec.statusURL = baseURL
		rec.statusToken = token
		return status, statusErr
	}
	quickstartBrowserAllowed = func() bool { return false }
	quickstartOpenBrowser = func(url string) error {
		rec.opened = append(rec.opened, url)
		return nil
	}
	// No server answers for sign-in links unless a test sets one.
	quickstartDevSignIn = func(baseURL, token, next string) (devSignInLink, error) {
		rec.signInCalls++
		return devSignInLink{}, errDevSignInUnavailable
	}
	return rec
}

func TestFetchOnboardingStatus(t *testing.T) {
	var gotAuth, gotPath string
	body := `{"complete":false,"embeddedBrokerID":""}`
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	client := srv.Client()

	t.Run("not complete", func(t *testing.T) {
		got, err := fetchOnboardingStatus(client, srv.URL, "tok")
		require.NoError(t, err)
		assert.False(t, got.Complete)
		assert.Empty(t, got.EmbeddedBrokerID)
		assert.Equal(t, "Bearer tok", gotAuth)
		assert.Equal(t, onboardingStatusPath, gotPath)
	})

	t.Run("complete, no token", func(t *testing.T) {
		body = `{"complete":true,"embeddedBrokerID":"b1"}`
		got, err := fetchOnboardingStatus(client, srv.URL, "")
		require.NoError(t, err)
		assert.True(t, got.Complete)
		assert.Equal(t, "b1", got.EmbeddedBrokerID)
		assert.Empty(t, gotAuth, "no Authorization header without a token")
	})

	t.Run("non-200 is an error", func(t *testing.T) {
		code = http.StatusNotFound
		defer func() { code = http.StatusOK }()
		_, err := fetchOnboardingStatus(client, srv.URL, "tok")
		assert.Error(t, err)
	})

	t.Run("bad body is an error", func(t *testing.T) {
		body = `not json`
		_, err := fetchOnboardingStatus(client, srv.URL, "tok")
		assert.Error(t, err)
	})

	t.Run("unreachable is an error", func(t *testing.T) {
		dead := httptest.NewServer(http.NotFoundHandler())
		url := dead.URL
		dead.Close()
		_, err := fetchOnboardingStatus(&http.Client{Timeout: time.Second}, url, "tok")
		assert.Error(t, err)
	})
}

func TestQuickstartWebPath(t *testing.T) {
	statusErr := errors.New("boom")
	tests := []struct {
		name            string
		status          onboardingStatusResult
		err             error
		settingsMissing bool
		want            string
	}{
		{"complete", onboardingStatusResult{Complete: true}, nil, false, ""},
		{"complete ignores settings signal", onboardingStatusResult{Complete: true}, nil, true, ""},
		{"not complete", onboardingStatusResult{Complete: false}, nil, false, "/onboarding"},
		{"status failed, settings existed: web root", onboardingStatusResult{}, statusErr, false, ""},
		{"status failed, settings missing: onboarding", onboardingStatusResult{}, statusErr, true, "/onboarding"},
		{"not asked, settings missing: onboarding", onboardingStatusResult{}, errOnboardingStatusNotAsked, true, "/onboarding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, quickstartWebPath(tt.status, tt.err, tt.settingsMissing))
		})
	}
}

func TestWorkstationBrokerRegistryCheck(t *testing.T) {
	regErr := errors.New("image_registry is not configured")
	missing := func() error { return regErr }
	present := func() error { return nil }
	notCalled := func() error {
		t.Fatal("registry check must not run when broker is disabled or co-located with hub/web")
		return nil
	}
	tests := []struct {
		name     string
		broker   bool
		hubOrWeb bool
		check    func() error
		wantErr  bool
	}{
		{"broker disabled", false, true, notCalled, false},
		{"co-located workstation, registry set", true, true, notCalled, false},
		{"co-located workstation, registry missing: broker still starts", true, true, notCalled, false},
		{"broker-only, registry missing: fail fast", true, false, missing, true},
		{"broker-only, registry set", true, false, present, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := workstationBrokerRegistryCheck(tt.broker, tt.hubOrWeb, tt.check)
			if tt.wantErr {
				assert.ErrorIs(t, err, regErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestBrowserAutoOpenAllowedFor(t *testing.T) {
	assert.True(t, browserAutoOpenAllowedFor("", true, false))
	assert.False(t, browserAutoOpenAllowedFor("1", true, false), "SCION_NO_BROWSER set")
	assert.False(t, browserAutoOpenAllowedFor("", false, false), "not a terminal")
	assert.False(t, browserAutoOpenAllowedFor("", true, true), "headless")
}

func TestPrintWorkstationQuickstart_OnboardingStatus(t *testing.T) {
	// The registry fallback (requireImageRegistryForBroker) must see a
	// registry so only the hub's answer drives the notice.
	t.Setenv("SCION_IMAGE_REGISTRY", "ghcr.io/test")
	statusErr := errors.New("boom")

	tests := []struct {
		name            string
		ready           bool
		status          onboardingStatusResult
		err             error
		settingsMissing bool
		hubEnabled      bool
		wantURL         string
		wantCalls       int
	}{
		{"complete: web root", true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil, true, true, "Web UI:  http://127.0.0.1:8080\n", 1},
		{"not complete: onboarding", true, onboardingStatusResult{EmbeddedBrokerID: "b1"}, nil, false, true, "Web UI:  http://127.0.0.1:8080/onboarding\n", 1},
		{"status fails, settings existed: web root", true, onboardingStatusResult{}, statusErr, false, true, "Web UI:  http://127.0.0.1:8080\n", 1},
		{"status fails, settings missing: onboarding", true, onboardingStatusResult{}, statusErr, true, true, "Web UI:  http://127.0.0.1:8080/onboarding\n", 1},
		{"not ready: status not asked, fallback", false, onboardingStatusResult{Complete: true}, nil, true, true, "Web UI:  http://127.0.0.1:8080/onboarding\n", 0},
		{"hub disabled: status not asked, fallback", true, onboardingStatusResult{Complete: true}, nil, false, false, "Web UI:  http://127.0.0.1:8080\n", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "dev-token"), []byte("scion_dev_tok\n"), 0600))
			rec := stubQuickstartProbes(t, tt.ready, tt.status, tt.err)

			out := captureStdout(t, func() {
				printWorkstationQuickstart(tt.settingsMissing, dir, nil, "127.0.0.1", 8080, true, false, tt.hubEnabled, true)
			})

			assert.Contains(t, out, tt.wantURL)
			assert.Equal(t, tt.wantCalls, rec.statusCalls)
			if tt.wantCalls > 0 {
				assert.Equal(t, "http://127.0.0.1:8080", rec.statusURL)
				assert.Equal(t, "scion_dev_tok", rec.statusToken, "status call uses the dev token file")
			}
			assert.Empty(t, rec.opened, "browser not opened when the rule disallows it")
		})
	}
}

func TestPrintWorkstationQuickstart_BrowserRule(t *testing.T) {
	t.Setenv("SCION_IMAGE_REGISTRY", "ghcr.io/test")

	t.Run("allowed and ready: opens the onboarding URL", func(t *testing.T) {
		rec := stubQuickstartProbes(t, true, onboardingStatusResult{EmbeddedBrokerID: "b1"}, nil)
		quickstartBrowserAllowed = func() bool { return true }
		_ = captureStdout(t, func() {
			printWorkstationQuickstart(false, t.TempDir(), nil, "127.0.0.1", 8080, true, false, true, true)
		})
		assert.Equal(t, []string{"http://127.0.0.1:8080/onboarding"}, rec.opened)
	})

	t.Run("allowed but not ready: does not open", func(t *testing.T) {
		rec := stubQuickstartProbes(t, false, onboardingStatusResult{}, nil)
		quickstartBrowserAllowed = func() bool { return true }
		_ = captureStdout(t, func() {
			printWorkstationQuickstart(true, t.TempDir(), nil, "127.0.0.1", 8080, true, false, true, true)
		})
		assert.Empty(t, rec.opened)
	})

	t.Run("disallowed (e.g. SCION_NO_BROWSER): prints, does not open", func(t *testing.T) {
		rec := stubQuickstartProbes(t, true, onboardingStatusResult{EmbeddedBrokerID: "b1"}, nil)
		out := captureStdout(t, func() {
			printWorkstationQuickstart(false, t.TempDir(), nil, "127.0.0.1", 8080, true, false, true, true)
		})
		assert.Contains(t, out, "/onboarding")
		assert.Empty(t, rec.opened)
	})
}

// unsetImageRegistry makes requireImageRegistryForBroker fail: no registry
// env vars and an empty HOME and working directory.
func unsetImageRegistry(t *testing.T) {
	t.Helper()
	t.Setenv("SCION_IMAGE_REGISTRY", "")
	t.Setenv("SCION_MAINTENANCE_IMAGE_REGISTRY", "")
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Chdir(tmpHome)
}

func TestPrintWorkstationQuickstart_NoBrokerSkippedNoticeWithoutRegistry(t *testing.T) {
	unsetImageRegistry(t)
	stubQuickstartProbes(t, true, onboardingStatusResult{EmbeddedBrokerID: "b1"}, nil)
	out := captureStdout(t, func() {
		printWorkstationQuickstart(false, t.TempDir(), nil, "127.0.0.1", 8080, true, false, true, true)
	})
	assert.Contains(t, out, "/onboarding")
	assert.NotContains(t, out, "Runtime broker not started")
	assert.NotContains(t, out, "scion server restart")
}
