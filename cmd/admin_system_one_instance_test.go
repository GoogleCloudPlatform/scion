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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/metrics"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	systemoneruntime "github.com/GoogleCloudPlatform/scion/pkg/systemone/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetAdminSystemOneFlags(t *testing.T) {
	t.Helper()
	prevLoader := systemOneAdminClientLoader
	prevFormat := outputFormat
	prevCreate := adminSystemOneCreateOpts
	prevList := adminSystemOneListOpts
	prevProbe := adminSystemOneProbeOpts

	resetFlagSet := func(cmd *cobra.Command) {
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(f.DefValue)
			f.Changed = false
		})
	}
	for _, c := range []*cobra.Command{
		adminSystemOneInstanceCreateCmd,
		adminSystemOneInstanceListCmd,
		adminSystemOneInstanceDeleteCmd,
		adminSystemOneInstanceProbeCmd,
	} {
		resetFlagSet(c)
	}
	adminSystemOneCreateOpts.authScopes = nil
	adminSystemOneCreateOpts.options = nil
	adminSystemOneProbeOpts.params = nil

	t.Cleanup(func() {
		systemOneAdminClientLoader = prevLoader
		outputFormat = prevFormat
		adminSystemOneCreateOpts = prevCreate
		adminSystemOneListOpts = prevList
		adminSystemOneProbeOpts = prevProbe
		for _, c := range []*cobra.Command{
			adminSystemOneInstanceCreateCmd,
			adminSystemOneInstanceListCmd,
			adminSystemOneInstanceDeleteCmd,
			adminSystemOneInstanceProbeCmd,
		} {
			resetFlagSet(c)
			c.SetIn(nil)
			c.SetOut(nil)
			c.SetErr(nil)
		}
	})
}

func useMockSystemOneServer(t *testing.T, handler http.Handler) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c, err := hubclient.New(srv.URL, hubclient.WithBearerToken("test-session-jwt"))
	require.NoError(t, err)
	systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
		return c.SystemOne(), nil
	}
}

func executeAdminSystemOneSubcmd(t *testing.T, args []string, stdin io.Reader) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rootCmd.SetArgs(append([]string{"admin", "system-one", "instance"}, args...))
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	if stdin != nil {
		rootCmd.SetIn(stdin)
	} else {
		rootCmd.SetIn(strings.NewReader(""))
	}
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetIn(nil)
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
	})

	// Ensure outside-project execution works without any ambient project state.
	home := t.TempDir()
	t.Setenv("HOME", home)

	err := rootCmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestAdminSystemOneCreate_KeyFromFileAndStdin_NeverInOutput(t *testing.T) {
	sentinelKey := "ts-synthetic-test-secret-do-not-print-98765"
	now := time.Date(2026, 11, 15, 20, 30, 0, 0, time.UTC)

	t.Run("from --key-file human output", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		var captured hubclient.CreateSystemOneInstanceRequest
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/api/v1/admin/system-one/instances", r.URL.Path)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.SystemOneInstance{
				ID:        "inst-001",
				Name:      captured.Name,
				Adapter:   captured.Adapter,
				Model:     captured.Model,
				Endpoint:  "https://api.typesafe.ai",
				Auth:      hubclient.SystemOneInstanceAuthDTO{Kind: "api_key", SecretName: "systemone.instance.inst-001"},
				Enabled:   true,
				Revision:  1,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}))

		keyFile := filepath.Join(t.TempDir(), "key.txt")
		require.NoError(t, os.WriteFile(keyFile, []byte(sentinelKey+"\n"), 0600))

		stdout, stderr, err := executeAdminSystemOneSubcmd(t, []string{
			"create", "typesafe-main",
			"--adapter", "typesafe",
			"--model", "jev-1.14.0",
			"--key-file", keyFile,
			"--option", "profile=fast",
			"--max-in-flight", "4",
			"--call-timeout", "5s",
		}, nil)
		require.NoError(t, err)
		require.NotNil(t, captured.Auth)
		assert.Equal(t, sentinelKey, captured.Auth.APIKey)
		assert.Equal(t, map[string]string{"profile": "fast"}, captured.Options)
		require.NotNil(t, captured.Limits)
		assert.Equal(t, 4, captured.Limits.MaxInFlight)
		assert.Equal(t, "5s", captured.Limits.CallTimeout)

		assert.Contains(t, stdout, "Created System One instance typesafe-main")
		assert.Contains(t, stdout, "systemone.instance.inst-001")
		assert.NotContains(t, stdout, sentinelKey)
		assert.NotContains(t, stderr, sentinelKey)
	})

	t.Run("from stdin with --format json", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		outputFormat = "json"
		var gotKey string
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req hubclient.CreateSystemOneInstanceRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			if req.Auth != nil {
				gotKey = req.Auth.APIKey
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.SystemOneInstance{
				ID:        "inst-002",
				Name:      req.Name,
				Adapter:   req.Adapter,
				Model:     req.Model,
				Endpoint:  "https://api.typesafe.ai",
				Auth:      hubclient.SystemOneInstanceAuthDTO{Kind: "api_key", SecretName: "systemone.instance.inst-002"},
				Enabled:   true,
				Revision:  1,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}))

		stdout, stderr, err := executeAdminSystemOneSubcmd(t, []string{
			"create", "typesafe-stdin",
			"--model", "jev-1.14.0",
			"--key-stdin",
		}, strings.NewReader(sentinelKey+"\r\n"))
		require.NoError(t, err)
		assert.Equal(t, sentinelKey, gotKey)
		assert.NotContains(t, stdout, sentinelKey)
		assert.NotContains(t, stderr, sentinelKey)

		var parsed hubclient.SystemOneInstance
		require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
		assert.Equal(t, "inst-002", parsed.ID)
		assert.Equal(t, "systemone.instance.inst-002", parsed.Auth.SecretName)
	})
}

func TestAdminSystemOneCreate_RejectsArgvFlagsAndEnvKeySources(t *testing.T) {
	sentinelKey := "ts-forbidden-argv-secret-55555"

	var httpCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name string
		args []string
	}{
		{
			name: "extra positional argument",
			args: []string{"create", "ts-main", sentinelKey, "--model", "jev-1.14.0"},
		},
		{
			name: "--api-key=value flag",
			args: []string{"create", "ts-main", "--model", "jev-1.14.0", "--api-key=" + sentinelKey},
		},
		{
			name: "--api-key separate arg flag",
			args: []string{"create", "ts-main", "--model", "jev-1.14.0", "--api-key", sentinelKey},
		},
		{
			name: "--key=value flag",
			args: []string{"create", "ts-main", "--model", "jev-1.14.0", "--key=" + sentinelKey},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAdminSystemOneFlags(t)
			useMockSystemOneServer(t, srv.Config.Handler)

			stdout, stderr, err := executeAdminSystemOneSubcmd(t, tc.args, strings.NewReader(""))
			require.Error(t, err)
			assert.Zero(t, httpCalls.Load(), "must reject before making any Hub request")
			assert.NotContains(t, err.Error(), sentinelKey, "error must never echo secret")
			assert.NotContains(t, stdout, sentinelKey, "stdout must never echo secret")
			assert.NotContains(t, stderr, sentinelKey, "stderr must never echo secret")
		})
	}

	t.Run("environment variables are never read for API key", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		useMockSystemOneServer(t, srv.Config.Handler)
		t.Setenv("API_KEY", sentinelKey)
		t.Setenv("TYPESAFE_API_KEY", sentinelKey)
		t.Setenv("SYSTEM_ONE_API_KEY", sentinelKey)

		stdout, stderr, err := executeAdminSystemOneSubcmd(t, []string{
			"create", "ts-main", "--model", "jev-1.14.0",
		}, strings.NewReader(""))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "API key is empty")
		assert.Zero(t, httpCalls.Load())
		assert.NotContains(t, err.Error(), sentinelKey)
		assert.NotContains(t, stdout, sentinelKey)
		assert.NotContains(t, stderr, sentinelKey)
	})

	t.Run("multiline or control-char key rejected without echoing", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		useMockSystemOneServer(t, srv.Config.Handler)

		stdout, stderr, err := executeAdminSystemOneSubcmd(t, []string{
			"create", "ts-main", "--model", "jev-1.14.0", "--key-stdin",
		}, strings.NewReader(sentinelKey+"\nextra-line"))
		require.Error(t, err)
		assert.Zero(t, httpCalls.Load())
		assert.NotContains(t, err.Error(), sentinelKey)
		assert.NotContains(t, stdout, sentinelKey)
		assert.NotContains(t, stderr, sentinelKey)
	})

	t.Run("upstream error containing key is redacted", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		systemOneAdminClientLoader = func() (hubclient.SystemOneService, error) {
			return errEchoingSystemOneService{errMsg: "proxy failure echoing " + sentinelKey}, nil
		}

		stdout, stderr, err := executeAdminSystemOneSubcmd(t, []string{
			"create", "ts-main", "--model", "jev-1.14.0", "--key-stdin",
		}, strings.NewReader(sentinelKey))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "[REDACTED]")
		assert.NotContains(t, err.Error(), sentinelKey)
		assert.NotContains(t, stdout, sentinelKey)
		assert.NotContains(t, stderr, sentinelKey)
	})
}

type errEchoingSystemOneService struct {
	hubclient.SystemOneService
	errMsg string
}

func (s errEchoingSystemOneService) CreateInstance(_ context.Context, _ *hubclient.CreateSystemOneInstanceRequest) (*hubclient.SystemOneInstance, error) {
	return nil, errors.New(s.errMsg)
}

func TestAdminSystemOneListAndDelete(t *testing.T) {
	now := time.Date(2026, 11, 15, 21, 0, 0, 0, time.UTC)

	t.Run("list empty", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.ListSystemOneInstancesResponse{})
		}))
		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{"list"}, nil)
		require.NoError(t, err)
		assert.Contains(t, stdout, "No System One instances configured.")
	})

	t.Run("list table with filters and cursor", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		var gotQuery string
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.ListSystemOneInstancesResponse{
				Instances: []hubclient.SystemOneInstance{
					{
						ID:        "inst-10",
						Name:      "ts-prod",
						Adapter:   "typesafe",
						Model:     "jev-1.14.0",
						Endpoint:  "https://api.typesafe.ai",
						Auth:      hubclient.SystemOneInstanceAuthDTO{Kind: "api_key", SecretName: "systemone.instance.inst-10"},
						Enabled:   false,
						UpdatedAt: now,
					},
				},
				NextCursor: "next-page-2",
				TotalCount: 2,
			})
		}))
		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{
			"list", "--adapter", "typesafe", "--enabled=false", "--limit", "1", "--cursor", "page-1",
		}, nil)
		require.NoError(t, err)
		assert.Contains(t, gotQuery, "adapter=typesafe")
		assert.Contains(t, gotQuery, "enabled=false")
		assert.Contains(t, gotQuery, "limit=1")
		assert.Contains(t, gotQuery, "cursor=page-1")
		assert.Contains(t, stdout, "ts-prod")
		assert.Contains(t, stdout, "systemone.instance.inst-10")
		assert.Contains(t, stdout, "Next cursor: next-page-2")
	})

	t.Run("delete human and json", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		var deletedPaths []string
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodDelete, r.Method)
			deletedPaths = append(deletedPaths, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{"delete", "ts-prod"}, nil)
		require.NoError(t, err)
		assert.Contains(t, stdout, "Deleted System One instance ts-prod")

		resetAdminSystemOneFlags(t)
		outputFormat = "json"
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			deletedPaths = append(deletedPaths, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}))
		jsonOut, _, err := executeAdminSystemOneSubcmd(t, []string{"delete", "ts-prod"}, nil)
		require.NoError(t, err)
		assert.JSONEq(t, `{"deleted":true,"name":"ts-prod"}`, jsonOut)
		assert.Len(t, deletedPaths, 2)
	})
}

func TestAdminSystemOneProbe(t *testing.T) {
	t.Run("--models probe", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req hubclient.ProbeSystemOneInstanceRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.True(t, req.Models)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.ProbeSystemOneInstanceResponse{
				Class:  systemoneruntime.ClassSuccess,
				Models: []string{"jev-1.14.0", "jev-1.13.0"},
			})
		}))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{"probe", "ts-prod", "--models"}, nil)
		require.NoError(t, err)
		assert.Contains(t, stdout, "Class: success")
		assert.Contains(t, stdout, "Models (2):")
		assert.Contains(t, stdout, "jev-1.14.0")
	})

	t.Run("YAML --spec-file and --text-file human output", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		var captured hubclient.ProbeSystemOneInstanceRequest
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
			yes := true
			choice := "alpha"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.ProbeSystemOneInstanceResponse{
				Class: systemoneruntime.ClassSuccess,
				Decision: &hubclient.ProbeSystemOneDecision{
					Kind: spec.KindJudgments,
				},
				Evaluation: &systemoneruntime.Evaluation{
					EvalID:          "eval-probe-1",
					ConfiguredModel: "jev-1.14.0",
					ResolvedModel:   "jev-1.14-prod-20261001",
					Class:           systemoneruntime.ClassSuccess,
					Notes:           []string{provider.NoteModelAliasReported},
					Elapsed:         35 * time.Millisecond,
					Answers: map[string]systemoneruntime.AnswerRecord{
						"q_bin": {
							QuestionID:   "q_bin",
							Primitive:    spec.PrimitiveBinary,
							Status:       "answered",
							Bool:         &yes,
							Distribution: map[string]float64{"yes": 0.9, "no": 0.1},
							Metrics: map[string]metrics.Metric{
								metrics.NamePTop: {Value: 0.9},
							},
							NativeMetrics: map[string]metrics.Metric{
								"typesafe.confidence": {Value: 0.88},
							},
						},
						"q_choice": {
							QuestionID:   "q_choice",
							Primitive:    spec.PrimitiveChoice,
							Status:       "answered",
							Choice:       &choice,
							Distribution: map[string]float64{"alpha": 0.7, "beta": 0.2, "gamma": 0.1},
						},
					},
					Attempts: []systemoneruntime.Attempt{
						{
							Instance:        "ts-prod",
							Adapter:         "typesafe",
							Outcome:         systemoneruntime.AttemptSuccess,
							HTTPStatusClass: "2xx",
							Duration:        30 * time.Millisecond,
							Requests:        1,
						},
					},
				},
			})
		}))

		dir := t.TempDir()
		specPath := filepath.Join(dir, "spec.yaml")
		require.NoError(t, os.WriteFile(specPath, []byte("version: 1\nquestions:\n  - id: q_bin\n    primitive: binary\n    instructions: Is it synthetic?\n"), 0600))
		textPath := filepath.Join(dir, "sample.txt")
		require.NoError(t, os.WriteFile(textPath, []byte("Synthetic test text."), 0600))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{
			"probe", "ts-prod",
			"--spec-file", specPath,
			"--text-file", textPath,
			"--kind", "judgments",
			"--rule-id", "probe-rule-1",
			"--param", "env=test",
		}, nil)
		require.NoError(t, err)
		require.NotNil(t, captured.Probe)
		assert.Equal(t, "Synthetic test text.", captured.Probe.Text)
		assert.Equal(t, "judgments", captured.Kind)
		assert.Equal(t, "probe-rule-1", captured.RuleID)
		assert.Equal(t, map[string]string{"env": "test"}, captured.Params)
		assert.True(t, json.Valid(captured.Spec))

		assert.Contains(t, stdout, "Configured Model:     jev-1.14.0")
		assert.Contains(t, stdout, "Resolved Model:       jev-1.14-prod-20261001")
		assert.Contains(t, stdout, "Model Alias Reported: true")
		assert.Contains(t, stdout, "Usage:                usage unavailable")
		assert.Contains(t, stdout, "typesafe.confidence=0.8800")
		assert.Contains(t, stdout, "p_top=0.9000")
		assert.Contains(t, stdout, "instance=ts-prod adapter=typesafe outcome=success")
	})

	t.Run("renders model_unreported and usage unavailable vs measured usage without 0-token false report", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "spec.json")
		require.NoError(t, os.WriteFile(specPath, []byte(`{"version":1,"questions":[{"id":"is_damaged","type":"binary","instructions":"Check probe.text."}]}`), 0600))

		// Case 1: missing usage (usage_unreported + model_unreported) — human and JSON.
		resetAdminSystemOneFlags(t)
		yes := true
		missingResp := hubclient.ProbeSystemOneInstanceResponse{
			Class: systemoneruntime.ClassSuccess,
			Decision: &hubclient.ProbeSystemOneDecision{
				Kind: spec.KindJudgmentBool,
				Bool: &yes,
			},
			Evaluation: &systemoneruntime.Evaluation{
				EvalID:          "eval-gemini-unreported",
				ConfiguredModel: "gemini-decision-1",
				ResolvedModel:   "",
				Class:           systemoneruntime.ClassSuccess,
				Notes:           []string{provider.NoteModelUnreported, provider.NoteUsageUnreported},
				Answers: map[string]systemoneruntime.AnswerRecord{
					"is_damaged": {
						QuestionID:    "is_damaged",
						Primitive:     spec.PrimitiveBinary,
						Status:        provider.AnswerStatusAnswered,
						Bool:          &yes,
						Distribution:  map[string]float64{"true": 0.99, "false": 0.01},
						NativeMetrics: map[string]metrics.Metric{"gemini.confidence": {Value: 0.99}},
						Notes:         []string{provider.NoteClamped001To099},
					},
				},
			},
		}
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(missingResp)
		}))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{
			"probe", "gemini-main",
			"--spec-file", specPath,
			"--text", "Synthetic sample.",
		}, nil)
		require.NoError(t, err)
		assert.Contains(t, stdout, "Configured Model:     gemini-decision-1")
		assert.Contains(t, stdout, "Resolved Model:       unreported (model_unreported)")
		assert.Contains(t, stdout, "Usage:                usage unavailable (usage_unreported)")
		assert.Contains(t, stdout, "Notes:                model_unreported, usage_unreported")
		assert.Contains(t, stdout, "notes: clamped_0.01_0.99")
		assert.NotContains(t, stdout, "input_tokens=0")
		assert.NotContains(t, stdout, "output_tokens=0")

		resetAdminSystemOneFlags(t)
		outputFormat = "json"
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(missingResp)
		}))
		jsonOut, _, err := executeAdminSystemOneSubcmd(t, []string{
			"probe", "gemini-main",
			"--spec-file", specPath,
			"--text", "Synthetic sample.",
		}, nil)
		require.NoError(t, err)
		assert.NotContains(t, jsonOut, `"usage"`)
		assert.Contains(t, jsonOut, `"model_unreported"`)
		assert.Contains(t, jsonOut, `"usage_unreported"`)

		// Case 2: measured usage present (total_input_tokens=58, total_output_tokens=0).
		resetAdminSystemOneFlags(t)
		measuredResp := missingResp
		measuredEval := *missingResp.Evaluation
		measuredEval.Notes = []string{provider.NoteModelUnreported}
		measuredEval.Usage = &provider.Usage{InputTokens: 58, OutputTokens: 0}
		measuredResp.Evaluation = &measuredEval
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(measuredResp)
		}))
		measuredOut, _, err := executeAdminSystemOneSubcmd(t, []string{
			"probe", "gemini-main",
			"--spec-file", specPath,
			"--text", "Synthetic sample.",
		}, nil)
		require.NoError(t, err)
		assert.Contains(t, measuredOut, "Resolved Model:       unreported (model_unreported)")
		assert.Contains(t, measuredOut, "Usage:                input_tokens=58, output_tokens=0")
	})

	t.Run("non-zero exit when class is not success", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			class  systemoneruntime.Class
			reason string
			format string
		}{
			{
				name:   "failed human output",
				class:  systemoneruntime.ClassFailed,
				reason: "provider_http_5xx",
				format: "",
			},
			{
				name:   "uncertain human output",
				class:  systemoneruntime.ClassUncertain,
				reason: "low_confidence",
				format: "",
			},
			{
				name:   "failed json output",
				class:  systemoneruntime.ClassFailed,
				reason: "provider_auth_rejected",
				format: "json",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetAdminSystemOneFlags(t)
				restoreAllSilenceUsage(t)
				outputFormat = tc.format
				useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(hubclient.ProbeSystemOneInstanceResponse{
						Class:  tc.class,
						Reason: tc.reason,
					})
				}))

				stdout, _, err := executeAdminSystemOneSubcmd(t, []string{"probe", "ts-prod", "--models"}, nil)
				require.Error(t, err)
				assert.Equal(t, 1, exitCodeFor(err))
				assert.Contains(t, err.Error(), string(tc.class))
				assert.Contains(t, err.Error(), tc.reason)
				if tc.format == "json" {
					assert.True(t, isReportedInJSON(err), "JSON mode must suppress duplicate stderr banner")
					var parsed hubclient.ProbeSystemOneInstanceResponse
					require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
					assert.Equal(t, tc.class, parsed.Class)
					assert.Equal(t, tc.reason, parsed.Reason)
				} else {
					assert.False(t, isReportedInJSON(err))
					assert.Contains(t, stdout, "Class: "+string(tc.class))
					assert.Contains(t, stdout, "Reason: "+tc.reason)
				}
			})
		}
	})

	t.Run("help omits unsupported judgment_score kind and documents gemini_decisions", func(t *testing.T) {
		kindFlag := adminSystemOneInstanceProbeCmd.Flags().Lookup("kind")
		require.NotNil(t, kindFlag)
		assert.NotContains(t, kindFlag.Usage, "judgment_score")

		resetAdminSystemOneFlags(t)
		createHelp, _, err := executeAdminSystemOneSubcmd(t, []string{"create", "--help"}, nil)
		require.NoError(t, err)
		assert.Contains(t, createHelp, "--adapter gemini_decisions")
		assert.Contains(t, createHelp, "--model gemini-decision-1")
		assert.Contains(t, createHelp, "--key-stdin")
		assert.Contains(t, createHelp, "--auth-kind")
		assert.Contains(t, createHelp, "--auth-scope")
		assert.Contains(t, createHelp, "--quota-project")
		assert.Contains(t, createHelp, "unverified (U-G9)")
		assert.Contains(t, createHelp, "cloud-platform alone was observed insufficient")
		assert.Contains(t, createHelp, "Generative Language API")
		assert.Contains(t, createHelp, "Hub restart")
		assert.NotContains(t, createHelp, "generative-language.retriever")
		assert.NotContains(t, createHelp, "--api-key")

		resetAdminSystemOneFlags(t)
		probeHelp, _, err := executeAdminSystemOneSubcmd(t, []string{"probe", "--help"}, nil)
		require.NoError(t, err)
		assert.Contains(t, probeHelp, "gemini-main")
		assert.Contains(t, probeHelp, "gemini_decisions")
		assert.Contains(t, probeHelp, "adapter does not support model listing")
	})

	t.Run("flag mutual exclusion", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "spec.json")
		require.NoError(t, os.WriteFile(specPath, []byte(`{"version":1}`), 0600))
		textPath := filepath.Join(dir, "text.txt")
		require.NoError(t, os.WriteFile(textPath, []byte("synthetic"), 0600))

		for _, tc := range []struct {
			name string
			args []string
		}{
			{"neither models nor spec", []string{"probe", "ts-prod"}},
			{"models with spec", []string{"probe", "ts-prod", "--models", "--spec-file", specPath}},
			{"models with text", []string{"probe", "ts-prod", "--models", "--text", "hi"}},
			{"spec without text", []string{"probe", "ts-prod", "--spec-file", specPath}},
			{"both text and text-file", []string{"probe", "ts-prod", "--spec-file", specPath, "--text", "hi", "--text-file", textPath}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetAdminSystemOneFlags(t)
				_, _, err := executeAdminSystemOneSubcmd(t, tc.args, nil)
				require.Error(t, err)
			})
		}
	})
}

func TestAdminSystemOne_AgentModeHidden(t *testing.T) {
	t.Setenv("SCION_CLI_MODE", "agent")
	require.Equal(t, ModeAgent, resolveMode())

	root, _ := gateTestTree(t)
	// Control: commands exist on the cloned real tree before mode restriction.
	for _, path := range []string{
		"admin",
		"admin.system-one",
		"admin.system-one.instance",
		"admin.system-one.instance.create",
		"admin.system-one.instance.list",
		"admin.system-one.instance.delete",
		"admin.system-one.instance.probe",
	} {
		require.NotNil(t, resolveCommandPath(root, path), "control: %s must exist before agent mode restriction", path)
		assert.False(t, agentAllowed[path], "%s must not be in agentAllowed", path)
	}

	applyModeRestrictions(root, ModeAgent)

	for _, path := range []string{
		"admin",
		"admin.system-one",
		"admin.system-one.instance",
		"admin.system-one.instance.create",
		"admin.system-one.instance.list",
		"admin.system-one.instance.delete",
		"admin.system-one.instance.probe",
	} {
		assert.Nil(t, resolveCommandPath(root, path), "%s must be removed in agent mode", path)
	}
}

func TestAdminSystemOne_WindowsGateAllowsSystemOneWhileGatingPromote(t *testing.T) {
	for _, tc := range []struct {
		args []string
		path string
	}{
		{[]string{"admin", "system-one", "instance", "create"}, "admin.system-one.instance.create"},
		{[]string{"admin", "system-one", "instance", "list"}, "admin.system-one.instance.list"},
		{[]string{"admin", "system-one", "instance", "delete"}, "admin.system-one.instance.delete"},
		{[]string{"admin", "system-one", "instance", "probe"}, "admin.system-one.instance.probe"},
	} {
		t.Run("allows "+tc.path, func(t *testing.T) {
			root, ran := gateTestTree(t)
			applyPlatformRestrictions(root, "windows")

			cmd := resolveCommandPath(root, tc.path)
			require.NotNil(t, cmd)
			assert.False(t, cmd.Hidden, "%s must stay visible on Windows", tc.path)
			assert.NoError(t, windowsRefusal(cmd))

			root.SetArgs(tc.args)
			require.NoError(t, root.Execute())
			assert.Equal(t, []string{tc.path}, *ran)
		})
	}

	t.Run("refuses admin promote on Windows", func(t *testing.T) {
		root, ran := gateTestTree(t)
		applyPlatformRestrictions(root, "windows")
		root.SetArgs([]string{"admin", "promote", "--email", "ops@example.com"})

		err := root.Execute()
		require.Error(t, err)
		assert.True(t, errors.Is(err, errUnsupportedOnWindows), "got %v", err)
		assert.Empty(t, *ran)
	})
}

func TestAdminSystemOneCreate_GoogleAccessTokenFlagsAndValidation(t *testing.T) {
	now := time.Date(2026, 11, 19, 18, 0, 0, 0, time.UTC)

	t.Run("google_access_token implicit quota project human output", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		var captured hubclient.CreateSystemOneInstanceRequest
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodPost, r.Method)
			require.Equal(t, "/api/v1/admin/system-one/instances", r.URL.Path)
			require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.SystemOneInstance{
				ID:       "inst-adc-1",
				Name:     captured.Name,
				Adapter:  captured.Adapter,
				Model:    captured.Model,
				Endpoint: "https://generativelanguage.googleapis.com",
				Auth: hubclient.SystemOneInstanceAuthDTO{
					Kind:                string(provider.AuthKindGoogleAccessToken),
					Scopes:              captured.Auth.Scopes,
					QuotaProject:        "implicit (service account association)",
					CredentialType:      "service_account",
					ServiceAccountEmail: "so-adc@proj-a.iam.gserviceaccount.com",
				},
				Enabled:   true,
				Revision:  1,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{
			"create", "gemini-adc",
			"--adapter", "gemini_decisions",
			"--model", "gemini-decision-1",
			"--auth-kind", "google_access_token",
			"--auth-scope", "https://www.googleapis.com/auth/scope-alpha",
			"--auth-scope", "https://www.googleapis.com/auth/scope-beta",
			"--auth-scope", "https://www.googleapis.com/auth/scope-alpha",
		}, nil)
		require.NoError(t, err)
		require.NotNil(t, captured.Auth)
		assert.Equal(t, "google_access_token", captured.Auth.Kind)
		assert.Empty(t, captured.Auth.APIKey)
		assert.Equal(t, []string{
			"https://www.googleapis.com/auth/scope-alpha",
			"https://www.googleapis.com/auth/scope-beta",
		}, captured.Auth.Scopes)
		assert.Empty(t, captured.Options)

		assert.Contains(t, stdout, "Created System One instance gemini-adc")
		assert.Contains(t, stdout, "Auth Kind:       google_access_token")
		assert.Contains(t, stdout, "Scopes:          https://www.googleapis.com/auth/scope-alpha, https://www.googleapis.com/auth/scope-beta")
		assert.Contains(t, stdout, "Quota Project:   implicit (service account association)")
		assert.Contains(t, stdout, "Credential Type: service_account")
		assert.Contains(t, stdout, "Service Account: so-adc@proj-a.iam.gserviceaccount.com")
		assert.NotContains(t, stdout, "Secret:")
	})

	t.Run("google_access_token explicit quota project json and human output", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		outputFormat = "json"
		var captured hubclient.CreateSystemOneInstanceRequest
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.SystemOneInstance{
				ID:       "inst-adc-2",
				Name:     captured.Name,
				Adapter:  captured.Adapter,
				Model:    captured.Model,
				Endpoint: "https://generativelanguage.googleapis.com",
				Auth: hubclient.SystemOneInstanceAuthDTO{
					Kind:           string(provider.AuthKindGoogleAccessToken),
					Scopes:         captured.Auth.Scopes,
					QuotaProject:   "explicit: billing-proj-01",
					CredentialType: "authorized_user",
				},
				Options:   captured.Options,
				Enabled:   true,
				Revision:  1,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{
			"create", "gemini-adc-qp",
			"--adapter", "gemini_decisions",
			"--model", "gemini-decision-1",
			"--auth-kind", "google_access_token",
			"--auth-scope", "https://www.googleapis.com/auth/scope-alpha",
			"--quota-project", "billing-proj-01",
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"quota_project": "billing-proj-01"}, captured.Options)

		var parsed hubclient.SystemOneInstance
		require.NoError(t, json.Unmarshal([]byte(stdout), &parsed))
		assert.Equal(t, "google_access_token", parsed.Auth.Kind)
		assert.Equal(t, "explicit: billing-proj-01", parsed.Auth.QuotaProject)
		assert.Equal(t, "authorized_user", parsed.Auth.CredentialType)
	})

	t.Run("pre-request flag conflict and validation errors make zero HTTP calls", func(t *testing.T) {
		var httpCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		keyFile := filepath.Join(t.TempDir(), "dummy-key.txt")
		require.NoError(t, os.WriteFile(keyFile, []byte("synthetic-key-do-not-send\n"), 0o600))

		for _, tc := range []struct {
			name    string
			args    []string
			stdin   string
			wantErr string
		}{
			{
				name: "google_access_token with --key-stdin",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://www.googleapis.com/auth/scope-a",
					"--key-stdin",
				},
				stdin:   "synthetic-stdin-key",
				wantErr: "--key-stdin and --key-file cannot be combined with --auth-kind google_access_token",
			},
			{
				name: "google_access_token with --key-file",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://www.googleapis.com/auth/scope-a",
					"--key-file", keyFile,
				},
				wantErr: "--key-stdin and --key-file cannot be combined with --auth-kind google_access_token",
			},
			{
				name: "google_access_token missing --auth-scope",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
				},
				wantErr: "--auth-scope is required with --auth-kind google_access_token",
			},
			{
				name: "google_access_token invalid --auth-scope prefix",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://evil.example/scope",
				},
				wantErr: "must start with https://www.googleapis.com/auth/",
			},
			{
				name: "google_access_token bare --auth-scope prefix",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://www.googleapis.com/auth/",
				},
				wantErr: "must start with https://www.googleapis.com/auth/",
			},
			{
				name: "google_access_token --auth-scope with query",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://www.googleapis.com/auth/scope-a?x=1",
				},
				wantErr: "disallowed URL characters",
			},
			{
				name: "google_access_token invalid --quota-project",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://www.googleapis.com/auth/scope-a",
					"--quota-project", "BAD_PROJECT",
				},
				wantErr: "invalid --quota-project",
			},
			{
				name: "google_access_token empty --quota-project=",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://www.googleapis.com/auth/scope-a",
					"--quota-project=",
				},
				wantErr: "invalid --quota-project",
			},
			{
				name: "google_access_token conflicting --quota-project and --option quota_project",
				args: []string{
					"create", "g-adc", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://www.googleapis.com/auth/scope-a",
					"--quota-project", "billing-proj-01",
					"--option", "quota_project=billing-proj-02",
				},
				wantErr: "conflicting --quota-project and --option quota_project",
			},
			{
				name: "api_key with --auth-scope",
				args: []string{
					"create", "g-key", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-scope", "https://www.googleapis.com/auth/scope-a",
					"--key-file", keyFile,
				},
				wantErr: "--auth-scope is only supported with --auth-kind google_access_token",
			},
			{
				name: "api_key with --quota-project",
				args: []string{
					"create", "g-key", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--quota-project", "billing-proj-01",
					"--key-file", keyFile,
				},
				wantErr: "--quota-project is only supported with --auth-kind google_access_token",
			},
			{
				name: "api_key with --option quota_project",
				args: []string{
					"create", "g-key", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--option", "quota_project=billing-proj-01",
					"--key-file", keyFile,
				},
				wantErr: "option quota_project is only supported with --auth-kind google_access_token",
			},
			{
				name: "unsupported --auth-kind",
				args: []string{
					"create", "g-bad", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "oauth_refresh",
				},
				wantErr: "unsupported --auth-kind",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetAdminSystemOneFlags(t)
				useMockSystemOneServer(t, srv.Config.Handler)

				_, _, err := executeAdminSystemOneSubcmd(t, tc.args, strings.NewReader(tc.stdin))
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Zero(t, httpCalls.Load(), "CLI validation error must happen before any HTTP request")
			})
		}
	})
}

func TestAdminSystemOneListAndProbe_ADCMetadataOutput(t *testing.T) {
	now := time.Date(2026, 11, 19, 18, 15, 0, 0, time.UTC)

	t.Run("list renders ADC auth kind, scopes, quota project provenance, and credential info", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.ListSystemOneInstancesResponse{
				Instances: []hubclient.SystemOneInstance{
					{
						ID:       "inst-adc-imp",
						Name:     "gemini-adc-imp",
						Adapter:  "gemini_decisions",
						Model:    "gemini-decision-1",
						Endpoint: "https://generativelanguage.googleapis.com",
						Auth: hubclient.SystemOneInstanceAuthDTO{
							Kind:                "google_access_token",
							Scopes:              []string{"https://www.googleapis.com/auth/scope-a"},
							QuotaProject:        "implicit (service account association)",
							CredentialType:      "service_account",
							ServiceAccountEmail: "so-sa@proj.iam.gserviceaccount.com",
						},
						Enabled:   true,
						UpdatedAt: now,
					},
					{
						ID:       "inst-adc-exp",
						Name:     "gemini-adc-exp",
						Adapter:  "gemini_decisions",
						Model:    "gemini-decision-1",
						Endpoint: "https://generativelanguage.googleapis.com",
						Auth: hubclient.SystemOneInstanceAuthDTO{
							Kind:           "google_access_token",
							Scopes:         []string{"https://www.googleapis.com/auth/scope-a"},
							QuotaProject:   "explicit: billing-proj-01",
							CredentialType: "metadata",
						},
						Options:   map[string]string{"quota_project": "billing-proj-01"},
						Enabled:   true,
						UpdatedAt: now,
					},
				},
				TotalCount: 2,
			})
		}))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{"list"}, nil)
		require.NoError(t, err)
		assert.Contains(t, stdout, "AUTH")
		assert.Contains(t, stdout, "SCOPES")
		assert.Contains(t, stdout, "QUOTA PROJECT")
		assert.Contains(t, stdout, "CREDENTIAL")
		assert.Contains(t, stdout, "google_access_token")
		assert.Contains(t, stdout, "implicit (service account association)")
		assert.Contains(t, stdout, "explicit: billing-proj-01")
		assert.Contains(t, stdout, "service_account (so-sa@proj.iam.gserviceaccount.com)")
		assert.Contains(t, stdout, "metadata")
	})

	t.Run("probe renders ADC evaluation auth provenance in human and json modes", func(t *testing.T) {
		dir := t.TempDir()
		specPath := filepath.Join(dir, "spec.json")
		require.NoError(t, os.WriteFile(specPath, []byte(`{"version":1,"questions":[{"id":"q1","type":"binary","instructions":"Check."}]}`), 0o600))

		yes := true
		probeResp := hubclient.ProbeSystemOneInstanceResponse{
			Class: systemoneruntime.ClassSuccess,
			Decision: &hubclient.ProbeSystemOneDecision{
				Kind: spec.KindJudgmentBool,
				Bool: &yes,
			},
			Evaluation: &systemoneruntime.Evaluation{
				EvalID:          "eval-adc-1",
				ConfiguredModel: "gemini-decision-1",
				Class:           systemoneruntime.ClassSuccess,
				Notes:           []string{provider.NoteModelUnreported, provider.NoteUsageUnreported},
				Auth: &systemoneruntime.AuthProvenance{
					AuthKind:            string(provider.AuthKindGoogleAccessToken),
					Scopes:              []string{"https://www.googleapis.com/auth/scope-a"},
					QuotaProject:        "implicit (service account association)",
					CredentialType:      "service_account",
					ServiceAccountEmail: "so-sa@proj.iam.gserviceaccount.com",
				},
			},
		}

		resetAdminSystemOneFlags(t)
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(probeResp)
		}))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{
			"probe", "gemini-adc-imp",
			"--spec-file", specPath,
			"--text", "Synthetic sample.",
		}, nil)
		require.NoError(t, err)
		assert.Contains(t, stdout, "Auth Kind:            google_access_token")
		assert.Contains(t, stdout, "Auth Scopes:          https://www.googleapis.com/auth/scope-a")
		assert.Contains(t, stdout, "Quota Project:        implicit (service account association)")
		assert.Contains(t, stdout, "Credential Type:      service_account")
		assert.Contains(t, stdout, "Service Account:      so-sa@proj.iam.gserviceaccount.com")

		resetAdminSystemOneFlags(t)
		outputFormat = "json"
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(probeResp)
		}))

		jsonOut, _, err := executeAdminSystemOneSubcmd(t, []string{
			"probe", "gemini-adc-imp",
			"--spec-file", specPath,
			"--text", "Synthetic sample.",
		}, nil)
		require.NoError(t, err)
		var parsed hubclient.ProbeSystemOneInstanceResponse
		require.NoError(t, json.Unmarshal([]byte(jsonOut), &parsed))
		require.NotNil(t, parsed.Evaluation)
		require.NotNil(t, parsed.Evaluation.Auth)
		assert.Equal(t, "google_access_token", parsed.Evaluation.Auth.AuthKind)
		assert.Equal(t, "implicit (service account association)", parsed.Evaluation.Auth.QuotaProject)
		assert.Equal(t, "service_account", parsed.Evaluation.Auth.CredentialType)
		assert.Equal(t, "so-sa@proj.iam.gserviceaccount.com", parsed.Evaluation.Auth.ServiceAccountEmail)
	})
}

func TestAdminSystemOne_NamedSecretFlagsOutputAndValidation(t *testing.T) {
	now := time.Date(2026, 11, 19, 23, 30, 0, 0, time.UTC)

	t.Run("gemini_decisions create defaults to GEMINI_API_KEY and project_or_hub", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		var captured hubclient.CreateSystemOneInstanceRequest
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&captured))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.SystemOneInstance{
				ID:       "inst-named-1",
				Name:     captured.Name,
				Adapter:  captured.Adapter,
				Model:    captured.Model,
				Endpoint: "https://generativelanguage.googleapis.com",
				Auth: hubclient.SystemOneInstanceAuthDTO{
					Kind:      "api_key",
					SecretRef: captured.Auth.SecretRef,
				},
				Enabled:   true,
				Revision:  1,
				CreatedAt: now,
				UpdatedAt: now,
			})
		}))

		stdout, _, err := executeAdminSystemOneSubcmd(t, []string{
			"create", "gemini-named",
			"--adapter", "gemini_decisions",
			"--model", "gemini-decision-1",
		}, strings.NewReader(""))
		require.NoError(t, err)
		require.NotNil(t, captured.Auth)
		assert.Empty(t, captured.Auth.APIKey)
		require.NotNil(t, captured.Auth.SecretRef)
		assert.Equal(t, "GEMINI_API_KEY", captured.Auth.SecretRef.Name)
		assert.Equal(t, "project_or_hub", captured.Auth.SecretRef.Scope)
		assert.Contains(t, stdout, "Secret Ref:      GEMINI_API_KEY (scope: project_or_hub)")
	})

	t.Run("help documents --secret-name, --secret-scope, exposure semantics, and probe --project", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		createHelp, _, err := executeAdminSystemOneSubcmd(t, []string{"create", "--help"}, nil)
		require.NoError(t, err)
		assert.Contains(t, createHelp, "--secret-name")
		assert.Contains(t, createHelp, "--secret-scope")
		assert.Contains(t, createHelp, "project_or_hub")
		assert.Contains(t, createHelp, "--type variable")
		assert.Contains(t, createHelp, "injection mode \"always\"")

		resetAdminSystemOneFlags(t)
		probeHelp, _, err := executeAdminSystemOneSubcmd(t, []string{"probe", "--help"}, nil)
		require.NoError(t, err)
		assert.Contains(t, probeHelp, "--project")
	})

	t.Run("pre-request mutual exclusion and grammar errors make zero HTTP calls", func(t *testing.T) {
		var httpCalls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)

		keyFile := filepath.Join(t.TempDir(), "k.txt")
		require.NoError(t, os.WriteFile(keyFile, []byte("synthetic-key\n"), 0o600))

		for _, tc := range []struct {
			name    string
			args    []string
			stdin   string
			wantErr string
		}{
			{
				name: "--secret-name with --key-stdin",
				args: []string{
					"create", "g1", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--secret-name", "GEMINI_API_KEY", "--key-stdin",
				},
				stdin:   "inline-secret",
				wantErr: "cannot be combined with --key-stdin or --key-file",
			},
			{
				name: "--secret-scope with --key-file",
				args: []string{
					"create", "g1", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--secret-scope", "hub", "--key-file", keyFile,
				},
				wantErr: "cannot be combined with --key-stdin or --key-file",
			},
			{
				name: "--secret-name with --auth-kind google_access_token",
				args: []string{
					"create", "g1", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--auth-kind", "google_access_token",
					"--auth-scope", "https://www.googleapis.com/auth/scope-a",
					"--secret-name", "GEMINI_API_KEY",
				},
				wantErr: "cannot be combined with --auth-kind google_access_token",
			},
			{
				name: "invalid --secret-name grammar",
				args: []string{
					"create", "g1", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--secret-name", "1BAD-NAME",
				},
				wantErr: "invalid --secret-name",
			},
			{
				name: "reserved prefix --secret-name refused",
				args: []string{
					"create", "g1", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--secret-name", "systemone_instance_other",
				},
				wantErr: "invalid --secret-name",
			},
			{
				name: "invalid --secret-scope refused",
				args: []string{
					"create", "g1", "--adapter", "gemini_decisions", "--model", "gemini-decision-1",
					"--secret-scope", "user",
				},
				wantErr: "invalid --secret-scope",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				resetAdminSystemOneFlags(t)
				useMockSystemOneServer(t, srv.Config.Handler)
				_, _, err := executeAdminSystemOneSubcmd(t, tc.args, strings.NewReader(tc.stdin))
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				assert.Zero(t, httpCalls.Load())
			})
		}
	})

	t.Run("list and probe --project render secret_ref scope, scope_used, and version", func(t *testing.T) {
		resetAdminSystemOneFlags(t)
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.ListSystemOneInstancesResponse{
				Instances: []hubclient.SystemOneInstance{
					{
						ID:       "inst-named-list",
						Name:     "gemini-named",
						Adapter:  "gemini_decisions",
						Model:    "gemini-decision-1",
						Endpoint: "https://generativelanguage.googleapis.com",
						Auth: hubclient.SystemOneInstanceAuthDTO{
							Kind:      "api_key",
							SecretRef: &hubclient.SystemOneSecretRefDTO{Name: "GEMINI_API_KEY", Scope: "project_or_hub"},
						},
						Enabled:   true,
						UpdatedAt: now,
					},
				},
				TotalCount: 1,
			})
		}))
		listOut, _, err := executeAdminSystemOneSubcmd(t, []string{"list"}, nil)
		require.NoError(t, err)
		assert.Contains(t, listOut, "GEMINI_API_KEY (project_or_hub)")

		dir := t.TempDir()
		specPath := filepath.Join(dir, "spec.json")
		require.NoError(t, os.WriteFile(specPath, []byte(`{"version":1,"questions":[{"id":"q1","type":"binary","instructions":"Check."}]}`), 0o600))

		var capturedProbe hubclient.ProbeSystemOneInstanceRequest
		yes := true
		resetAdminSystemOneFlags(t)
		useMockSystemOneServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.NoError(t, json.NewDecoder(r.Body).Decode(&capturedProbe))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.ProbeSystemOneInstanceResponse{
				Class:    systemoneruntime.ClassSuccess,
				Decision: &hubclient.ProbeSystemOneDecision{Kind: spec.KindJudgmentBool, Bool: &yes},
				Evaluation: &systemoneruntime.Evaluation{
					EvalID:          "eval-named-1",
					ConfiguredModel: "gemini-decision-1",
					Class:           systemoneruntime.ClassSuccess,
					Auth: &systemoneruntime.AuthProvenance{
						AuthKind: "api_key",
						SecretRef: &systemoneruntime.SecretRefProvenance{
							Name:      "GEMINI_API_KEY",
							Scope:     "project_or_hub",
							ScopeUsed: "project",
						},
						SecretVersion: 3,
					},
				},
			})
		}))
		probeOut, _, err := executeAdminSystemOneSubcmd(t, []string{
			"probe", "gemini-named",
			"--project", "my-project",
			"--spec-file", specPath,
			"--text", "Synthetic sample.",
		}, nil)
		require.NoError(t, err)
		assert.Equal(t, "my-project", capturedProbe.Project)
		assert.Contains(t, probeOut, "Secret Ref:           GEMINI_API_KEY (scope: project_or_hub, scope_used: project)")
		assert.Contains(t, probeOut, "Secret Version:       3")
	})
}
