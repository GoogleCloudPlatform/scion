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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func installMockMessagePolicyServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	hc, err := hubclient.New(srv.URL, hubclient.WithBearerToken("test-token"))
	require.NoError(t, err)

	origLoader := messagePolicyClientLoader
	messagePolicyClientLoader = func(*cobra.Command) (hubclient.MessagePolicyService, error) {
		return hc.ProjectMessagePolicies("proj-1"), nil
	}
	t.Cleanup(func() {
		messagePolicyClientLoader = origLoader
	})
}

func runMessagePolicyCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	origFormat := outputFormat
	outputFormat = ""
	t.Cleanup(func() { outputFormat = origFormat })

	tree := &cobra.Command{
		Use:           "scion",
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	tree.PersistentFlags().StringVarP(&outputFormat, "format", "", "", "Output format (json, plain)")
	tree.AddCommand(newMessagePolicyCmd())

	var out bytes.Buffer
	tree.SetOut(&out)
	tree.SetErr(&out)
	tree.SetArgs(append([]string{"message-policy"}, args...))
	err := tree.Execute()
	return out.String(), err
}

func TestMessagePolicyCLI_WindowsGateAllowsSubtree(t *testing.T) {
	for _, sub := range []string{"create", "list", "get", "update", "delete", "test", "decisions"} {
		path := "message-policy." + sub
		require.True(t, windowsAllowed[path], "%s must be in windowsAllowed", path)

		root, ran := gateTestTree(t)
		applyPlatformRestrictions(root, "windows")
		root.SetArgs([]string{"message-policy", sub})
		require.NoError(t, root.Execute(), "%s should be allowed by Windows gate", path)
		assert.Equal(t, []string{path}, *ran)
	}
}

func TestMessagePolicyCLI_HelpCoverageAndDefaults(t *testing.T) {
	out, err := runMessagePolicyCLI(t, "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "agent_to_user")
	assert.Contains(t, out, "agent_to_conversation")
	assert.Contains(t, out, "agent_dm")
	assert.Contains(t, out, "NOT evaluated")
	assert.NotContains(t, out, "M1", "user-facing help must not contain internal milestone labels")

	for _, sub := range []string{"create", "list", "get", "update", "delete", "test", "decisions"} {
		subHelp, subErr := runMessagePolicyCLI(t, sub, "--help")
		require.NoError(t, subErr)
		assert.NotContains(t, subHelp, "M1", "subcommand %s --help must not contain internal milestone labels", sub)
	}

	testHelp, err := runMessagePolicyCLI(t, "test", "--help")
	require.NoError(t, err)
	assert.Contains(t, testHelp, "(default: message)")
	assert.Contains(t, testHelp, "(default: inline-test)")
}

func TestMessagePolicyCLI_CreateListGetDelete_TextAndJSON(t *testing.T) {
	now := time.Date(2026, 11, 17, 7, 0, 0, 0, time.UTC)
	cov := messagepolicy.Coverage()

	samplePolicy := hubclient.MessagePolicyResponse{
		MessagePolicy: store.MessagePolicy{
			ID:                "pol-uuid-1",
			ProjectID:         "proj-1",
			Name:              "no-prose-choices",
			Description:       "Forbid prose choices",
			Enabled:           true,
			Mode:              "enforce",
			Scope:             store.MessagePolicyScope{Surfaces: []string{"agent_to_user"}},
			ViolationRule:     "!message.has_choices && message.body.contains('Option A')",
			OnFailed:          "reject",
			RejectReason:      "Prose choices are not permitted.",
			RejectRemediation: "Revise the message.",
			Revision:          1,
			CreatedAt:         now,
			UpdatedAt:         now,
		},
		Coverage: cov,
	}

	installMockMessagePolicyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/message-policies":
			var req hubclient.CreateMessagePolicyRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "no-prose-choices", req.Name)
			assert.Equal(t, []string{"agent_to_user"}, req.Scope.Surfaces)
			assert.Equal(t, "reject", req.OnFailed)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(samplePolicy)

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/message-policies":
			_ = json.NewEncoder(w).Encode(hubclient.ListMessagePoliciesResponse{
				Policies:   []hubclient.MessagePolicyResponse{samplePolicy},
				Coverage:   cov,
				TotalCount: 1,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/message-policies/no-prose-choices":
			_ = json.NewEncoder(w).Encode(samplePolicy)

		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-1/message-policies/no-prose-choices":
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	})

	// 1. Create (text)
	out, err := runMessagePolicyCLI(t, "create", "no-prose-choices",
		"--surface", "agent_to_user",
		"--mode", "enforce",
		"--on-failed", "reject",
		"--rule", "!message.has_choices && message.body.contains('Option A')",
		"--reject-reason", "Prose choices are not permitted.",
		"--reject-remediation", "Revise the message.",
	)
	require.NoError(t, err)
	assert.Contains(t, out, `Created message policy "no-prose-choices"`)
	assert.Contains(t, out, "Coverage (this Hub):")
	assert.NotContains(t, out, "M1")
	assert.Contains(t, out, "enforced_surfaces:     agent_to_user, agent_to_conversation")
	assert.Contains(t, out, "uncovered_agent_paths: agent_dm, group_post, group_set, mention_fanout, mention_delivery, broadcast")

	// 2. Create from YAML file with --format json
	specPath := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(specPath, []byte(`
name: no-prose-choices
mode: enforce
scope:
  surfaces:
    - agent_to_user
violation_rule: "!message.has_choices && message.body.contains('Option A')"
on_failed: reject
`), 0600))
	jsonOut, err := runMessagePolicyCLI(t, "--format", "json", "create", "--file", specPath)
	require.NoError(t, err)
	var decoded hubclient.MessagePolicyResponse
	require.NoError(t, json.Unmarshal([]byte(jsonOut), &decoded))
	assert.Equal(t, "pol-uuid-1", decoded.ID)
	assert.Equal(t, cov.EnforcedSurfaces, decoded.Coverage.EnforcedSurfaces)

	// 3. List (text + json)
	listOut, err := runMessagePolicyCLI(t, "list", "--mode", "enforce", "--enabled", "true")
	require.NoError(t, err)
	assert.Contains(t, listOut, "no-prose-choices")
	assert.Contains(t, listOut, "enforced_surfaces:     agent_to_user, agent_to_conversation")
	assert.Contains(t, listOut, "uncovered_agent_paths:")

	listJSON, err := runMessagePolicyCLI(t, "--format", "json", "list")
	require.NoError(t, err)
	var listDecoded hubclient.ListMessagePoliciesResponse
	require.NoError(t, json.Unmarshal([]byte(listJSON), &listDecoded))
	require.Len(t, listDecoded.Policies, 1)
	assert.Equal(t, cov.EnforcedSurfaces, listDecoded.Coverage.EnforcedSurfaces)

	// 4. Get (text + json)
	getOut, err := runMessagePolicyCLI(t, "get", "no-prose-choices")
	require.NoError(t, err)
	assert.Contains(t, getOut, "Name:               no-prose-choices")
	assert.Contains(t, getOut, "Violation Rule:     !message.has_choices")
	assert.Contains(t, getOut, "enforced_surfaces:     agent_to_user, agent_to_conversation")

	getJSON, err := runMessagePolicyCLI(t, "get", "no-prose-choices", "--json")
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal([]byte(getJSON), &decoded))
	assert.Equal(t, "no-prose-choices", decoded.Name)

	// 5. Delete (text + json)
	delOut, err := runMessagePolicyCLI(t, "delete", "no-prose-choices")
	require.NoError(t, err)
	assert.Contains(t, delOut, `Deleted message policy "no-prose-choices".`)

	delJSON, err := runMessagePolicyCLI(t, "--format", "json", "delete", "no-prose-choices")
	require.NoError(t, err)
	assert.Contains(t, delJSON, `"status": "deleted"`)
}

func TestMessagePolicyCLI_TestAndDecisions_TextAndJSON(t *testing.T) {
	largeBody := strings.Repeat("x", 17*1024)
	bodyPath := filepath.Join(t.TempDir(), "sample.txt")
	require.NoError(t, os.WriteFile(bodyPath, []byte(largeBody), 0600))

	installMockMessagePolicyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/message-policies/test":
			var req hubclient.TestMessagePolicyRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.NotNil(t, req.Sample)
			assert.Equal(t, len(largeBody), len(req.Sample.Body))
			_ = json.NewEncoder(w).Encode(hubclient.TestMessagePolicyResponse{
				PolicyName:    "inline-policy",
				Mode:          "enforce",
				Surface:       "agent_to_user",
				ScopeMatched:  true,
				Class:         "violation",
				Outcome:       "reject",
				Action:        "rejected",
				Reason:        "Rejected in dry run.",
				Remediation:   "Use structured options.",
				BodyBytes:     16384,
				BodyTruncated: true,
				ElapsedMs:     2,
			})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/message-policies/no-prose-choices/test":
			_ = json.NewEncoder(w).Encode(hubclient.TestMessagePolicyResponse{
				PolicyID:       "pol-1",
				PolicyName:     "no-prose-choices",
				PolicyRevision: 3,
				Mode:           "shadow",
				Surface:        "agent_to_user",
				ScopeMatched:   true,
				Class:          "pass",
				Outcome:        "admit",
				Action:         "shadow",
				BodyBytes:      12,
				ElapsedMs:      1,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/message-policies/decisions":
			assert.Equal(t, "rejected", r.URL.Query().Get("action"))
			_ = json.NewEncoder(w).Encode(hubclient.ListMessagePolicyDecisionsResponse{
				Decisions: []store.MessagePolicyDecision{
					{
						ID:            "dec-1",
						ProjectID:     "proj-1",
						PolicyID:      "pol-1",
						PolicyName:    "no-prose-choices",
						Mode:          "enforce",
						Surface:       "agent_to_user",
						SenderKind:    "agent",
						SenderID:      "ag-1",
						RecipientKind: "user",
						RecipientID:   "usr-1",
						Class:         "violation",
						Action:        "rejected",
						Reason:        "Prose choices are not permitted.",
						ElapsedMs:     1,
					},
				},
				TotalCount: 1,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/message-policies/no-prose-choices/decisions":
			_ = json.NewEncoder(w).Encode(hubclient.ListMessagePolicyDecisionsResponse{
				Decisions: []store.MessagePolicyDecision{
					{
						ID:            "dec-2",
						ProjectID:     "proj-1",
						PolicyID:      "pol-1",
						PolicyName:    "no-prose-choices",
						Mode:          "shadow",
						Surface:       "agent_to_user",
						SenderKind:    "agent",
						SenderID:      "ag-1",
						RecipientKind: "user",
						RecipientID:   "usr-1",
						MessageID:     "msg-42",
						Class:         "violation",
						Action:        "shadow",
						ElapsedMs:     1,
					},
				},
				TotalCount: 1,
			})

		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	})

	testOut, err := runMessagePolicyCLI(t, "test",
		"--rule", "message.body.size() > 10",
		"--on-failed", "reject",
		"--mode", "enforce",
		"--surface", "agent_to_user",
		"--body-file", bodyPath,
	)
	require.NoError(t, err)
	assert.Contains(t, testOut, "Class:           violation")
	assert.Contains(t, testOut, "Action:          rejected")
	assert.Contains(t, testOut, "Remediation:     Use structured options.")
	assert.Contains(t, testOut, "Body Bytes:      16384 (truncated: true)")
	assert.NotContains(t, testOut, largeBody[:64])

	storedJSON, err := runMessagePolicyCLI(t, "--format", "json", "test", "no-prose-choices", "--body", "hello world!")
	require.NoError(t, err)
	var testDecoded hubclient.TestMessagePolicyResponse
	require.NoError(t, json.Unmarshal([]byte(storedJSON), &testDecoded))
	assert.Equal(t, "pol-1", testDecoded.PolicyID)
	assert.Equal(t, "pass", testDecoded.Class)

	decOut, err := runMessagePolicyCLI(t, "decisions", "--action", "rejected")
	require.NoError(t, err)
	assert.Contains(t, decOut, "no-prose-choices")
	assert.Contains(t, decOut, "rejected")
	assert.Contains(t, decOut, "Prose choices are not permitted.")

	decJSON, err := runMessagePolicyCLI(t, "--format", "json", "decisions", "no-prose-choices")
	require.NoError(t, err)
	var decResp hubclient.ListMessagePolicyDecisionsResponse
	require.NoError(t, json.Unmarshal([]byte(decJSON), &decResp))
	require.Len(t, decResp.Decisions, 1)
	assert.Equal(t, "msg-42", decResp.Decisions[0].MessageID)
	assert.Equal(t, "shadow", decResp.Decisions[0].Action)
}

func TestMessagePolicyCLI_ErrorMapping(t *testing.T) {
	t.Run("400 invalid_rule includes category line and column", func(t *testing.T) {
		installMockMessagePolicyServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_rule","message":"undeclared reference to 'unknown_var'","details":{"field":"violation_rule","line":2,"column":14,"category":"unknown_variable"}}}`))
		})

		_, err := runMessagePolicyCLI(t, "create", "bad-rule",
			"--surface", "agent_to_user",
			"--on-failed", "reject",
			"--rule", "true &&\n             unknown_var",
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid_rule")
		assert.Contains(t, err.Error(), "category=unknown_variable")
		assert.Contains(t, err.Error(), "line=2")
		assert.Contains(t, err.Error(), "column=14")
		assert.Contains(t, err.Error(), "undeclared reference to 'unknown_var'")
	})

	t.Run("400 surface_not_covered renders server-supplied enforced_surfaces from error details", func(t *testing.T) {
		installMockMessagePolicyServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":"surface_not_covered","message":"surface \"broadcast\" is not covered","details":{"field":"scope.surfaces","enforced_surfaces":["agent_to_user","agent_to_conversation","agent_dm"]}}}`))
		})

		_, err := runMessagePolicyCLI(t, "create", "uncovered",
			"--surface", "broadcast",
			"--on-failed", "reject",
			"--rule", "true",
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "surface_not_covered (scope.surfaces)")
		assert.Contains(t, err.Error(), "Coverage (this Hub): agent_to_user, agent_to_conversation, agent_dm")
		assert.NotContains(t, err.Error(), "M1")
	})

	t.Run("list renders Hub-reported coverage rather than client compiled Coverage()", func(t *testing.T) {
		installMockMessagePolicyServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(hubclient.ListMessagePoliciesResponse{
				Policies: nil,
				Coverage: messagepolicy.CoverageInfo{
					EnforcedSurfaces:    []string{"agent_to_user", "agent_to_conversation", "agent_dm"},
					UncoveredAgentPaths: nil,
				},
			})
		})

		listOut, err := runMessagePolicyCLI(t, "list")
		require.NoError(t, err)
		assert.Contains(t, listOut, "Coverage (this Hub):")
		assert.Contains(t, listOut, "enforced_surfaces:     agent_to_user, agent_to_conversation, agent_dm")
		assert.Contains(t, listOut, "uncovered_agent_paths: (none)")
		assert.NotContains(t, listOut, "M1")
	})

	t.Run("404 route not found explains hub.message_policies experiment is off", func(t *testing.T) {
		installMockMessagePolicyServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"route not found"}}`))
		})

		_, err := runMessagePolicyCLI(t, "list")
		require.Error(t, err)
		assert.Equal(t, errMsgMessagePoliciesExperimentRequired, err.Error())
	})

	t.Run("404 missing policy preserves policy not found message", func(t *testing.T) {
		installMockMessagePolicyServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"Message policy not found"}}`))
		})

		_, err := runMessagePolicyCLI(t, "get", "missing-policy")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Message policy not found")
		assert.NotContains(t, err.Error(), "hub.message_policies")
	})
}

func TestMessagePolicyCLI_Test_RefusesNestedPolicyWithInlineOverrideFlags(t *testing.T) {
	nestedFile := filepath.Join(t.TempDir(), "nested-test.yaml")
	require.NoError(t, os.WriteFile(nestedFile, []byte(`
policy:
  name: nested-spec
  mode: shadow
  scope:
    surfaces:
      - agent_to_user
  violation_rule: "message.body.contains('blocked')"
  on_failed: admit
sample:
  surface: agent_to_user
  body: "blocked sample"
`), 0600))

	ruleFile := filepath.Join(t.TempDir(), "override.cel")
	require.NoError(t, os.WriteFile(ruleFile, []byte(`true`), 0600))

	serverCalls := 0
	installMockMessagePolicyServer(t, func(w http.ResponseWriter, r *http.Request) {
		serverCalls++
		w.Header().Set("Content-Type", "application/json")
		var req hubclient.TestMessagePolicyRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		_ = json.NewEncoder(w).Encode(hubclient.TestMessagePolicyResponse{
			PolicyName:   "nested-spec",
			Mode:         "shadow",
			Surface:      "agent_to_user",
			ScopeMatched: true,
			Class:        "violation",
			Outcome:      "reject",
			Action:       "shadow",
			BodyBytes:    len(req.Sample.Body),
		})
	})

	overrideFlagCases := [][]string{
		{"--mode", "enforce"},
		{"--rule", "false"},
		{"--rule-file", ruleFile},
		{"--name", "other-name"},
		{"--policy-surface", "agent_to_conversation"},
		{"--on-uncertain", "reject"},
		{"--on-failed", "reject"},
		{"--on-failed-reason", "timeout=reject"},
		{"--reject-reason", "override reason"},
		{"--reject-remediation", "override remediation"},
	}
	for _, flagArgs := range overrideFlagCases {
		args := append([]string{"test", "--file", nestedFile}, flagArgs...)
		_, err := runMessagePolicyCLI(t, args...)
		require.Error(t, err, "expected error for %v", flagArgs)
		assert.Contains(t, err.Error(), "nested", "error for %v: %v", flagArgs, err)
		assert.Contains(t, err.Error(), flagArgs[0], "error for %v: %v", flagArgs, err)
	}
	assert.Equal(t, 0, serverCalls, "refused flag combinations must not send HTTP requests")

	// Also refuse a request file setting both nested 'policy' and top-level policy fields.
	mixedFile := filepath.Join(t.TempDir(), "mixed-test.yaml")
	require.NoError(t, os.WriteFile(mixedFile, []byte(`
policy:
  violation_rule: "true"
  on_failed: admit
mode: enforce
sample:
  body: "hi"
`), 0600))
	_, err := runMessagePolicyCLI(t, "test", "--file", mixedFile)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nested")
	assert.Equal(t, 0, serverCalls)

	// Sample-only flags (e.g. --body) remain allowed alongside a nested 'policy' file.
	out, err := runMessagePolicyCLI(t, "test", "--file", nestedFile, "--body", "overridden body")
	require.NoError(t, err)
	assert.Contains(t, out, "Policy:          nested-spec")
	assert.Equal(t, 1, serverCalls)
}

func TestMessagePolicyCreateHelp_DocumentsContentTriggeredFailuresAndBodyTruncated(t *testing.T) {
	out, err := runMessagePolicyCLI(t, "create", "--help")
	require.NoError(t, err)
	assert.Contains(t, out, "body_truncated")
	assert.Contains(t, out, "cost_limit, timeout, budget_exhausted and body_truncated can be triggered by message content; admit lets such messages through.")
}

func TestMessagePolicyCLI_CreateUpdateGetSystemOneRule(t *testing.T) {
	cov := messagepolicy.Coverage()
	const s1Rule = `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"binding":{"instance":"ts-1"},"questions":[{"id":"asks_choice","type":"binary","instructions":"Asks choice?"}]}}`

	var (
		createdReq hubclient.CreateMessagePolicyRequest
		patchedReq hubclient.PatchMessagePolicyRequest
	)
	installMockMessagePolicyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/message-policies"):
			require.NoError(t, json.NewDecoder(r.Body).Decode(&createdReq))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(hubclient.MessagePolicyResponse{
				MessagePolicy: store.MessagePolicy{
					ID:            "pol-s1-1",
					ProjectID:     "proj-1",
					Name:          createdReq.Name,
					Enabled:       true,
					Mode:          "shadow",
					Scope:         createdReq.Scope,
					ViolationRule: createdReq.ViolationRule,
					SystemOneRule: createdReq.SystemOneRule,
					SystemOneRefs: []string{"ts-1"},
					OnUncertain:   createdReq.OnUncertain,
					OnFailed:      createdReq.OnFailed,
					Revision:      1,
				},
				Coverage: cov,
			})
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/message-policies/no-prose-s1"):
			require.NoError(t, json.NewDecoder(r.Body).Decode(&patchedReq))
			_ = json.NewEncoder(w).Encode(hubclient.MessagePolicyResponse{
				MessagePolicy: store.MessagePolicy{
					ID:            "pol-s1-1",
					ProjectID:     "proj-1",
					Name:          "no-prose-s1",
					Enabled:       true,
					Mode:          "enforce",
					Scope:         store.MessagePolicyScope{Surfaces: []string{"agent_to_user"}},
					ViolationRule: "!message.has_choices && judgment_flags.asks_choice",
					SystemOneRule: s1Rule,
					SystemOneRefs: []string{"ts-1"},
					OnUncertain:   "reject",
					OnFailed:      "reject",
					Revision:      2,
				},
				Coverage: cov,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	createOut, err := runMessagePolicyCLI(t,
		"create", "no-prose-s1",
		"--surface", "agent_to_user",
		"--rule", "!message.has_choices && judgment_flags.asks_choice",
		"--system-one-rule", s1Rule,
		"--on-uncertain", "admit",
		"--on-failed", "reject",
	)
	require.NoError(t, err)
	assert.Equal(t, s1Rule, createdReq.SystemOneRule)
	assert.Equal(t, "admit", createdReq.OnUncertain)
	assert.Contains(t, createOut, "System One Rule:")
	assert.Contains(t, createOut, "System One Refs:    ts-1")

	updateOut, err := runMessagePolicyCLI(t,
		"update", "no-prose-s1",
		"--mode", "enforce",
		"--on-uncertain", "reject",
		"--expected-revision", "1",
	)
	require.NoError(t, err)
	require.NotNil(t, patchedReq.Mode)
	assert.Equal(t, "enforce", *patchedReq.Mode)
	require.NotNil(t, patchedReq.OnUncertain)
	assert.Equal(t, "reject", *patchedReq.OnUncertain)
	assert.Equal(t, int64(1), patchedReq.ExpectedRevision)
	assert.Contains(t, updateOut, "Updated message policy \"no-prose-s1\"")
	assert.Contains(t, updateOut, "System One Refs:    ts-1")

	// Agent mode must keep message-policy.update hidden (only list and get allowed).
	assert.False(t, agentAllowed["message-policy.update"])
}
