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
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequestDevSignInLink(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	expires := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":       "abc",
			"signInPath": "/auth/dev-signin?code=abc",
			"expiresAt":  expires,
		})
	}))
	defer srv.Close()

	link, err := requestDevSignInLink(srv.Client(), srv.URL, fixtureDevToken, "/onboarding")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, devLaunchPath, gotPath)
	assert.Equal(t, "Bearer "+fixtureDevToken, gotAuth)
	assert.Equal(t, srv.URL+"/auth/dev-signin?code=abc&next=%2Fonboarding", link.URL)
	assert.True(t, link.ExpiresAt.Equal(expires))
	assert.NotContains(t, link.URL, fixtureDevToken, "the link never carries the dev token")

	link, err = requestDevSignInLink(srv.Client(), srv.URL+"/", fixtureDevToken, "")
	require.NoError(t, err)
	assert.Equal(t, srv.URL+"/auth/dev-signin?code=abc", link.URL)
}

func TestRequestDevSignInLink_Errors(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()

	_, err := requestDevSignInLink(srv.Client(), srv.URL, fixtureDevToken, "")
	assert.ErrorIs(t, err, errDevSignInUnavailable)

	status = http.StatusUnauthorized
	_, err = requestDevSignInLink(srv.Client(), srv.URL, fixtureDevToken, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "refused the dev token")

	_, err = requestDevSignInLink(srv.Client(), srv.URL, "", "")
	require.Error(t, err)
}

// stubServerOpen replaces runServerOpen's link and browser seams and points
// HOME at a temp dir holding a dev token file.
func stubServerOpen(t *testing.T, allowBrowser bool) (gotBase, gotToken *string, opened *[]string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SCION_SERVER_AUTH_DEVTOKEN", "")
	globalDir := filepath.Join(home, ".scion")
	require.NoError(t, os.MkdirAll(globalDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "dev-token"), []byte(fixtureDevToken+"\n"), 0o600))

	origLink, origAllowed, origOpen, origPrint, origTTY := serverOpenRequestLink, serverOpenBrowserAllowed, serverOpenBrowser, serverOpenPrintOnly, serverOpenStdoutIsTerminal
	t.Cleanup(func() {
		serverOpenRequestLink, serverOpenBrowserAllowed, serverOpenBrowser, serverOpenPrintOnly, serverOpenStdoutIsTerminal = origLink, origAllowed, origOpen, origPrint, origTTY
	})
	serverOpenStdoutIsTerminal = func() bool { return true }
	var base, token string
	var urls []string
	serverOpenRequestLink = func(baseURL, tok, next string) (devSignInLink, error) {
		base, token = baseURL, tok
		return devSignInLink{URL: baseURL + "/auth/dev-signin?code=xyz", ExpiresAt: time.Now().Add(2 * time.Minute)}, nil
	}
	serverOpenBrowserAllowed = func() bool { return allowBrowser }
	serverOpenBrowser = func(u string) error {
		urls = append(urls, u)
		return nil
	}
	serverOpenPrintOnly = false
	return &base, &token, &urls
}

func TestRunServerOpen_OpensSignInLink(t *testing.T) {
	base, token, opened := stubServerOpen(t, true)
	var out bytes.Buffer
	serverOpenCmd.SetOut(&out)
	t.Cleanup(func() { serverOpenCmd.SetOut(nil) })

	require.NoError(t, runServerOpen(serverOpenCmd, nil))
	assert.Equal(t, fixtureDevToken, *token, "the link is requested with the server's dev token")
	assert.True(t, strings.HasPrefix(*base, "http://"), *base)
	require.Equal(t, []string{*base + "/auth/dev-signin?code=xyz"}, *opened)
	assert.Contains(t, out.String(), "/auth/dev-signin?code=xyz")
	assert.NotContains(t, out.String(), fixtureDevToken)
}

func TestRunServerOpen_PrintOnly(t *testing.T) {
	_, _, opened := stubServerOpen(t, true)
	serverOpenPrintOnly = true
	var out bytes.Buffer
	serverOpenCmd.SetOut(&out)
	t.Cleanup(func() { serverOpenCmd.SetOut(nil) })

	require.NoError(t, runServerOpen(serverOpenCmd, nil))
	assert.Empty(t, *opened, "--print opens no browser")
	assert.Contains(t, out.String(), "Sign-in link (single use")
	assert.Contains(t, out.String(), "/auth/dev-signin?code=xyz")
}

func TestRunServerOpen_NoDevToken(t *testing.T) {
	stubServerOpen(t, false)
	require.NoError(t, os.Remove(filepath.Join(os.Getenv("HOME"), ".scion", "dev-token")))
	err := runServerOpen(serverOpenCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no dev token")
}

func TestRunServerOpen_DevAuthOff(t *testing.T) {
	stubServerOpen(t, false)
	serverOpenRequestLink = func(string, string, string) (devSignInLink, error) {
		return devSignInLink{}, errDevSignInUnavailable
	}
	err := runServerOpen(serverOpenCmd, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errDevSignInUnavailable))
}

// With dev auth, the quickstart prints the sign-in link and opens it, not
// the bare URL; the link is requested with the server's token and lands on
// the page the quickstart chose.
func TestPrintWorkstationQuickstart_OpensDevSignInLink(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dev-token"), []byte(fixtureDevToken+"\n"), 0o600))
	rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: false, EmbeddedBrokerID: "b1"}, nil)
	quickstartBrowserAllowed = func() bool { return true }
	var gotBase, gotToken, gotNext string
	quickstartDevSignIn = func(baseURL, token, next string) (devSignInLink, error) {
		rec.signInCalls++
		gotBase, gotToken, gotNext = baseURL, token, next
		return devSignInLink{URL: baseURL + "/auth/dev-signin?code=xyz&next=%2Fonboarding", ExpiresAt: time.Now().Add(2 * time.Minute)}, nil
	}

	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, dir, nil, "127.0.0.1", 8080, true, true, true, true)
	})

	assert.Equal(t, 1, rec.signInCalls)
	assert.Equal(t, "http://127.0.0.1:8080", gotBase)
	assert.Equal(t, fixtureDevToken, gotToken)
	assert.Equal(t, "/onboarding", gotNext)
	assert.Equal(t, []string{"http://127.0.0.1:8080/auth/dev-signin?code=xyz&next=%2Fonboarding"}, rec.opened)
	assert.Contains(t, output, "Web UI:  http://127.0.0.1:8080/onboarding")
	assert.Contains(t, output, "Sign in: http://127.0.0.1:8080/auth/dev-signin?code=xyz")
	assert.Contains(t, output, "scion server open")
	assert.NotContains(t, output, fixtureDevToken)
}

// When no link can be had, the quickstart says how to get one and opens
// the bare URL.
func TestPrintWorkstationQuickstart_DevSignInUnavailable(t *testing.T) {
	dir := t.TempDir()
	rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	quickstartBrowserAllowed = func() bool { return true }

	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, dir, nil, "127.0.0.1", 8080, true, true, true, true)
	})

	assert.Equal(t, 1, rec.signInCalls)
	assert.Equal(t, []string{"http://127.0.0.1:8080"}, rec.opened)
	assert.Contains(t, output, "To sign in to the web UI, run: scion server open")
	assert.NotContains(t, output, "Sign in: ")
}

// Without dev auth no sign-in link is requested.
func TestPrintWorkstationQuickstart_NoDevAuthNoSignInLink(t *testing.T) {
	rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, t.TempDir(), nil, "127.0.0.1", 8080, true, false, true, true)
	})
	assert.Zero(t, rec.signInCalls)
	assert.NotContains(t, output, "scion server open")
}

func TestRestartedWorkstation(t *testing.T) {
	assert.True(t, restartedWorkstation([]string{"server", "start", "--foreground"}, ""))
	assert.True(t, restartedWorkstation(nil, "workstation"))
	assert.False(t, restartedWorkstation([]string{"server", "start", "--foreground", "--hosted"}, ""))
	assert.False(t, restartedWorkstation([]string{"server", "start", "--production=true"}, ""))
	assert.False(t, restartedWorkstation([]string{"server", "start"}, "hosted"))
}

// Without a terminal on stdout, server open never prints the single-use link
// unless --print asks for it: after opening the browser it says so without
// the link, and with no browser it fails pointing at --print.
func TestRunServerOpen_LinkNotPrintedWithoutTerminal(t *testing.T) {
	base, _, opened := stubServerOpen(t, true)
	serverOpenStdoutIsTerminal = func() bool { return false }
	var out bytes.Buffer
	serverOpenCmd.SetOut(&out)
	t.Cleanup(func() { serverOpenCmd.SetOut(nil) })

	require.NoError(t, runServerOpen(serverOpenCmd, nil))
	require.Len(t, *opened, 1, "the browser still gets the link")
	assert.Contains(t, out.String(), "Opened the web UI in your browser.")
	assert.NotContains(t, out.String(), "/auth/dev-signin", "no link on a non-terminal stdout")

	out.Reset()
	*base = ""
	serverOpenBrowserAllowed = func() bool { return false }
	err := runServerOpen(serverOpenCmd, nil)
	require.Error(t, err)
	assert.Empty(t, *base, "no link is minted when there is nowhere to put it")
	assert.Contains(t, err.Error(), "--print")
	assert.NotContains(t, err.Error(), "/auth/dev-signin")
	assert.NotContains(t, out.String(), "/auth/dev-signin")

	out.Reset()
	serverOpenPrintOnly = true
	require.NoError(t, runServerOpen(serverOpenCmd, nil))
	assert.Contains(t, out.String(), "/auth/dev-signin?code=xyz", "--print prints the link on request")
}

// Without a terminal on stdout, the quickstart opens the link but prints
// only the 'scion server open' hint, so a redirected stdout or a log never
// holds the code.
func TestPrintWorkstationQuickstart_SignInLinkNotPrintedWithoutTerminal(t *testing.T) {
	rec := stubQuickstartProbes(t, true, onboardingStatusResult{Complete: true, EmbeddedBrokerID: "b1"}, nil)
	quickstartBrowserAllowed = func() bool { return true }
	quickstartStdoutIsTerminal = func() bool { return false }
	quickstartDevSignIn = func(baseURL, token, next string) (devSignInLink, error) {
		rec.signInCalls++
		return devSignInLink{URL: baseURL + "/auth/dev-signin?code=xyz", ExpiresAt: time.Now().Add(2 * time.Minute)}, nil
	}

	output := captureStdout(t, func() {
		printWorkstationQuickstart(false, t.TempDir(), nil, "127.0.0.1", 8080, true, true, true, true)
	})

	assert.Equal(t, 1, rec.signInCalls)
	assert.Equal(t, []string{"http://127.0.0.1:8080/auth/dev-signin?code=xyz"}, rec.opened, "the browser still gets the link")
	assert.NotContains(t, output, "code=xyz", "no link on a non-terminal stdout")
	assert.NotContains(t, output, "Sign in: ")
	assert.Contains(t, output, "To sign in to the web UI, run: scion server open")
}
