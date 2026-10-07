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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// TestBuiltinRestoreCommands_ModeAvailability pins D6 (ptone/scion#3544)
// against the real command tree: harness-config restore and templates
// restore are available in human and assistant mode and removed in agent
// mode.
func TestBuiltinRestoreCommands_ModeAvailability(t *testing.T) {
	paths := []string{"harness-config.restore", "templates.restore", "template.restore"}
	for _, p := range paths {
		require.NotNil(t, resolveCommandPath(rootCmd, p), "%s must exist in the real command tree", p)
		assert.False(t, agentAllowed[p], "agentAllowed must not contain %s (D6: agent mode denied)", p)
		assert.False(t, assistantDenied[p], "assistantDenied must not contain %s (D6: assistant mode allowed)", p)
	}

	build := func() *cobra.Command {
		root := &cobra.Command{Use: "scion"}
		for _, parent := range []string{"harness-config", "templates", "template"} {
			real := resolveCommandPath(rootCmd, parent)
			require.NotNil(t, real)
			root.AddCommand(cloneCommandShape(real))
		}
		return root
	}

	for _, tc := range []struct {
		mode string
		kept bool
	}{
		{"human", true},
		{"assistant", true},
		{"agent", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("SCION_CLI_MODE", tc.mode)
			root := build()
			applyModeRestrictions(root)
			for _, p := range paths {
				if tc.kept {
					assert.NotNil(t, resolveCommandPath(root, p), "%s must be available in %s mode", p, tc.mode)
				} else {
					assert.Nil(t, resolveCommandPath(root, p), "%s must be removed in %s mode", p, tc.mode)
				}
			}
		})
	}
}

// TestTemplateRestoreAliasSharesImplementation checks that the singular
// 'scion template restore' alias runs the same Args and RunE as
// 'scion templates restore'.
func TestTemplateRestoreAliasSharesImplementation(t *testing.T) {
	alias := resolveCommandPath(rootCmd, "template.restore")
	require.NotNil(t, alias)
	assert.Equal(t, reflect.ValueOf(templatesRestoreCmd.RunE).Pointer(), reflect.ValueOf(alias.RunE).Pointer())
	assert.Equal(t, reflect.ValueOf(templatesRestoreCmd.Args).Pointer(), reflect.ValueOf(alias.Args).Pointer())
	assert.NotNil(t, alias.Flags().Lookup("all"))
}

func TestBuiltinRestoreArgs(t *testing.T) {
	newCmd := func(allowNone bool) *cobra.Command {
		c := &cobra.Command{Use: "restore", Args: namesOrAllArgs("thing", allowNone), RunE: func(*cobra.Command, []string) error { return nil }}
		c.Flags().Bool("all", false, "")
		return c
	}
	for _, tc := range []struct {
		name      string
		allowNone bool
		args      []string
		wantErr   bool
	}{
		{"names", false, []string{"claude", "codex"}, false},
		{"all", false, []string{"--all"}, false},
		{"none rejected", false, nil, true},
		{"none allowed", true, nil, false},
		{"names and all", false, []string{"claude", "--all"}, true},
		{"names and all (allowNone)", true, []string{"default", "--all"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCmd(tc.allowNone)
			c.SetArgs(tc.args)
			c.SetOut(&bytes.Buffer{})
			c.SetErr(&bytes.Buffer{})
			err := c.Execute()
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRunBuiltinRestore_SendsRequestAndReports(t *testing.T) {
	var gotPath string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.Equal(t, http.MethodPost, r.Method)
		gotBody = nil
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"restored":       []string{"claude"},
			"alreadyPresent": []string{"codex"},
		})
	}))
	defer srv.Close()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	var out bytes.Buffer
	err = runBuiltinRestore(context.Background(), &out, "harness-config",
		client.HarnessConfigs().Restore, []string{"claude", "codex"}, false)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/harness-configs/restore", gotPath)
	assert.Equal(t, []interface{}{"claude", "codex"}, gotBody["names"])
	assert.NotContains(t, gotBody, "all")
	assert.Contains(t, out.String(), "Restored harness-config(s): claude.")
	assert.Contains(t, out.String(), "Already present, unchanged: codex.")

	out.Reset()
	err = runBuiltinRestore(context.Background(), &out, "template",
		client.Templates().Restore, nil, true)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/templates/restore", gotPath)
	assert.Equal(t, true, gotBody["all"])
	assert.NotContains(t, gotBody, "names")
}

func TestRunBuiltinRestore_ReportsHubError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{"code": "validation_error", "message": "not a built-in harness-config: nope"},
		})
	}))
	defer srv.Close()
	client, err := hubclient.New(srv.URL)
	require.NoError(t, err)

	err = runBuiltinRestore(context.Background(), &bytes.Buffer{}, "harness-config",
		client.HarnessConfigs().Restore, []string{"nope"}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a built-in harness-config: nope")
}
