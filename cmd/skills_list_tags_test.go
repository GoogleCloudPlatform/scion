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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSkillsListTags is the regression test for ptone/scion#2863: --tags was
// a single-value string flag, so repeating it kept only the last value. It
// is now a string slice; every value, repeated or comma-separated, reaches
// the list filter.
func TestSkillsListTags(t *testing.T) {
	resetTags := func() {
		f := skillsListCmd.Flags().Lookup("tags")
		require.NotNil(t, f)
		require.NoError(t, f.Value.(pflag.SliceValue).Replace(nil))
		f.Changed = false
	}
	t.Cleanup(resetTags)

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{"not set", nil, nil},
		{"single value", []string{"--tags", "go"}, []string{"go"}},
		{"repeated flag", []string{"--tags", "a", "--tags", "b"}, []string{"a", "b"}},
		{"comma-separated", []string{"--tags", "a,b"}, []string{"a", "b"}},
		{"repeated and comma-separated", []string{"--tags", "a,b", "--tags=c"}, []string{"a", "b", "c"}},
		{"blank entries dropped", []string{"--tags", "a, ,b,"}, []string{"a", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetTags()
			require.NoError(t, skillsListCmd.ParseFlags(tt.args))
			assert.Equal(t, tt.want, skillsListTags(skillsListCmd))
		})
	}
}

// TestRunSkillsList_TagsReachRequest runs skills list against a mock hub and
// checks every --tags value, repeated or comma-separated, reaches the list
// request as the hub's comma-joined tags query parameter.
func TestRunSkillsList_TagsReachRequest(t *testing.T) {
	origProjectPath := projectPath
	origFormat, origYes, origNonInteractive, origNoHub := outputFormat, autoConfirm, nonInteractive, noHub
	t.Cleanup(func() {
		projectPath = origProjectPath
		outputFormat, autoConfirm, nonInteractive, noHub = origFormat, origYes, origNonInteractive, origNoHub
	})

	var gotQueries []url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok"})
		case r.URL.Path == "/api/v1/skills" && r.Method == http.MethodGet:
			gotQueries = append(gotQueries, r.URL.Query())
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"skills": []interface{}{}})
		default:
			t.Logf("unhandled %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	hermeticCLIEnv(t)
	tmpHome := os.Getenv("HOME")
	t.Setenv("SCION_HUB_ENDPOINT", server.URL)
	projectPath = setupEnvProject(t, tmpHome, server.URL)
	outputFormat, autoConfirm, nonInteractive, noHub = "json", true, true, false

	f := skillsListCmd.Flags().Lookup("tags")
	require.NotNil(t, f)
	t.Cleanup(func() {
		_ = f.Value.(pflag.SliceValue).Replace(nil)
		f.Changed = false
	})
	require.NoError(t, f.Value.(pflag.SliceValue).Replace(nil))
	f.Changed = false
	require.NoError(t, skillsListCmd.ParseFlags([]string{"--tags", "a,b", "--tags", "c"}))

	_, _ = captureStdIO(t, func() {
		require.NoError(t, runSkillsList(skillsListCmd, nil))
	})

	require.Len(t, gotQueries, 1)
	assert.Equal(t, "a,b,c", gotQueries[0].Get("tags"))
}

// TestSkillsListTags_CSVQuoting pins pflag's CSV parsing of --tags: a bare
// double quote is a parse error. (A tag cannot contain a comma: the client
// joins tags with commas and the hub splits on them.)
func TestSkillsListTags_CSVQuoting(t *testing.T) {
	f := skillsListCmd.Flags().Lookup("tags")
	require.NotNil(t, f)
	reset := func() {
		require.NoError(t, f.Value.(pflag.SliceValue).Replace(nil))
		f.Changed = false
	}
	t.Cleanup(reset)

	reset()
	assert.Error(t, skillsListCmd.ParseFlags([]string{`--tags`, `a"b`}))
}
