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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubBrokerMappingSettings substitutes the global settings a joining broker
// reports its kubernetes_service_account_mappings from.
func stubBrokerMappingSettings(t *testing.T, vs *config.VersionedSettings, err error) {
	t.Helper()
	prev := loadBrokerMappingSettings
	t.Cleanup(func() { loadBrokerMappingSettings = prev })
	loadBrokerMappingSettings = func() (*config.VersionedSettings, error) { return vs, err }
}

func profileByName(t *testing.T, profiles []hubclient.BrokerProfile, name string) hubclient.BrokerProfile {
	t.Helper()
	for _, p := range profiles {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("profile %q not found", name)
	return hubclient.BrokerProfile{}
}

func TestBuildBrokerProfiles_ReportsSAMappings(t *testing.T) {
	stubBrokerMappingSettings(t, &config.VersionedSettings{
		Profiles: map[string]config.V1ProfileConfig{
			"gke":   {Runtime: "gke-entry", KubernetesServiceAccountMappings: map[string]string{"p@x.iam.gserviceaccount.com": "ksa-p"}},
			"empty": {Runtime: "k8s"},
			"local": {Runtime: "docker"},
		},
		Runtimes: map[string]config.V1RuntimeConfig{
			// A custom entry key whose type is kubernetes counts.
			"gke-entry": {Type: "kubernetes", KubernetesServiceAccountMappings: map[string]string{"r@x.iam.gserviceaccount.com": "ksa-r"}},
			"k8s":       {},
			"docker":    {},
		},
	}, nil)

	profiles := buildBrokerProfiles(&config.Settings{Profiles: map[string]config.ProfileConfig{
		"gke":     {Runtime: "gke-entry"},
		"empty":   {Runtime: "k8s"},
		"local":   {Runtime: "docker"},
		"missing": {Runtime: "k8s"}, // not in the global settings
	}})

	gke := profileByName(t, profiles, "gke")
	assert.True(t, gke.MappingsReported)
	assert.Equal(t, []hubclient.BrokerProfileSAMapping{
		{GSA: "p@x.iam.gserviceaccount.com"}, {GSA: "r@x.iam.gserviceaccount.com"},
	}, gke.ServiceAccountMappings, "profile mapping plus its runtime entry's")

	empty := profileByName(t, profiles, "empty")
	assert.True(t, empty.MappingsReported, "a Kubernetes profile reports, with nothing mapped")
	assert.Empty(t, empty.ServiceAccountMappings)

	local := profileByName(t, profiles, "local")
	assert.False(t, local.MappingsReported, "only Kubernetes profiles report")
	assert.False(t, profileByName(t, profiles, "missing").MappingsReported, "a profile the settings lack is unknown")

	// Wire shape: additive, omitempty fields.
	b, err := json.Marshal(gke)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"serviceAccountMappings":[{"gsa":"p@x.iam.gserviceaccount.com"}`)
	assert.Contains(t, string(b), `"mappingsReported":true`)
	b, err = json.Marshal(local)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "mappingsReported")
}

func TestBuildBrokerProfiles_UnreadableSettingsReportUnknown(t *testing.T) {
	stubBrokerMappingSettings(t, nil, errors.New("bad settings"))

	profiles := buildBrokerProfiles(&config.Settings{Profiles: map[string]config.ProfileConfig{"gke": {Runtime: "k8s"}}})
	gke := profileByName(t, profiles, "gke")
	assert.False(t, gke.MappingsReported, "unreadable settings must read as unknown, not as nothing mapped")
	assert.Empty(t, gke.ServiceAccountMappings)
	b, err := json.Marshal(gke)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "mappingsReported")
	assert.NotContains(t, string(b), "serviceAccountMappings")
}

func doctorSAMappingsServer(t *testing.T, items int, warnings []string) hubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/gcp-service-accounts", r.URL.Path)
		assert.Equal(t, "project", r.URL.Query().Get("scope"))
		assert.Equal(t, "proj-1", r.URL.Query().Get("scopeId"))
		list := make([]hubclient.GCPServiceAccount, items)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": list, "warnings": warnings})
	}))
	t.Cleanup(srv.Close)
	c, err := hubclient.New(srv.URL)
	require.NoError(t, err)
	return c
}

func TestCheckDoctorSAMappings(t *testing.T) {
	t.Run("warn lists each unmapped account", func(t *testing.T) {
		c := doctorSAMappingsServer(t, 2, []string{"GCP service account a@x is not mapped"})
		res := checkDoctorSAMappings("http://hub", true, c, "proj-1")
		assert.Equal(t, "warn", res.Status)
		assert.Contains(t, res.Message, "1 of 2")
		assert.Contains(t, res.Message, "a@x is not mapped")
		assert.Contains(t, res.Remediation, "kubernetes_service_account_mappings")
	})
	t.Run("pass without warnings", func(t *testing.T) {
		c := doctorSAMappingsServer(t, 2, nil)
		res := checkDoctorSAMappings("http://hub", true, c, "proj-1")
		assert.Equal(t, "pass", res.Status)
	})
	t.Run("skips", func(t *testing.T) {
		assert.Equal(t, "skip", checkDoctorSAMappings("", true, nil, "proj-1").Status)
		assert.Equal(t, "skip", checkDoctorSAMappings("http://hub", false, nil, "proj-1").Status)
		assert.Equal(t, "skip", checkDoctorSAMappings("http://hub", true, nil, "proj-1").Status)
		c := doctorSAMappingsServer(t, 0, nil)
		assert.Equal(t, "skip", checkDoctorSAMappings("http://hub", true, c, "").Status)
	})
	t.Run("list error is a warning, never a failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"code":"internal","message":"boom"}}`, http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		c, err := hubclient.New(srv.URL)
		require.NoError(t, err)
		res := checkDoctorSAMappings("http://hub", true, c, "proj-1")
		assert.Equal(t, "warn", res.Status)
	})
}

// captureStdoutStderr runs fn with os.Stdout and os.Stderr redirected and
// returns what each received.
func captureStdoutStderr(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	rOut, wOut, err := os.Pipe()
	require.NoError(t, err)
	rErr, wErr, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout, os.Stderr = wOut, wErr
	outCh, errCh := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(rOut); outCh <- string(b) }()
	go func() { b, _ := io.ReadAll(rErr); errCh <- string(b) }()
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()
	fn()
	_ = wOut.Close()
	_ = wErr.Close()
	return <-outCh, <-errCh
}

// The service-account commands print the Hub's warnings to stderr; with
// --json, stdout carries only the JSON document.
func TestProjectServiceAccountCommands_WarningsOnStderr(t *testing.T) {
	const warning = "GCP service account a@p.iam.gserviceaccount.com is not mapped"
	sa := map[string]interface{}{"id": "sa-1", "email": "a@p.iam.gserviceaccount.com", "warnings": []string{warning}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": []interface{}{sa}, "warnings": []string{warning}})
		case strings.HasSuffix(r.URL.Path, "/mint"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(sa)
		case strings.HasSuffix(r.URL.Path, "/verify"):
			_ = json.NewEncoder(w).Encode(sa)
		default: // create
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(sa)
		}
	}))
	t.Cleanup(srv.Close)
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	prevResolve, prevFormat, prevListJSON := resolveProjectForSA, outputFormat, saOutputJSON
	t.Cleanup(func() { resolveProjectForSA, outputFormat, saOutputJSON = prevResolve, prevFormat, prevListJSON })
	resolveProjectForSA = func() (hubclient.Client, string, error) { return client, "proj-1", nil }

	cases := []struct {
		name string
		run  func() error
		json func(bool)
	}{
		{"add", func() error { return runSAAdd(nil, []string{"a@p.iam.gserviceaccount.com"}) }, func(b bool) { outputFormat = map[bool]string{true: "json", false: ""}[b] }},
		{"mint", func() error { return runSAMint(nil, nil) }, func(b bool) { outputFormat = map[bool]string{true: "json", false: ""}[b] }},
		{"verify", func() error { return runSAVerify(nil, []string{"sa-1"}) }, func(b bool) { outputFormat = map[bool]string{true: "json", false: ""}[b] }},
		{"list", func() error { return runSAList(nil, nil) }, func(b bool) { saOutputJSON = b }},
	}
	for _, tc := range cases {
		for _, asJSON := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s json=%v", tc.name, asJSON), func(t *testing.T) {
				tc.json(asJSON)
				var runErr error
				stdout, stderr := captureStdoutStderr(t, func() { runErr = tc.run() })
				require.NoError(t, runErr, "warnings never fail the command")
				assert.Contains(t, stderr, "Warning: "+warning)
				assert.NotContains(t, stdout, "Warning: ")
				if asJSON {
					var v interface{}
					assert.NoError(t, json.Unmarshal([]byte(stdout), &v), "stdout must be one clean JSON document: %q", stdout)
				}
			})
		}
	}
}
