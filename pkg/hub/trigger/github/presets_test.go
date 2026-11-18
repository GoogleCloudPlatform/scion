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

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/triggerrule"
	"github.com/GoogleCloudPlatform/scion/pkg/triggertemplate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAckCommentSlot = "github.ackComment.body"

var registerTestAckSlotOnce sync.Once

func ensureTestAckCommentSlot(t *testing.T) {
	t.Helper()
	registerTestAckSlotOnce.Do(func() {
		if _, exists := triggertemplate.LookupSlot(testAckCommentSlot); exists {
			return
		}
		err := triggertemplate.Register(triggertemplate.Slot{
			Name:     testAckCommentSlot,
			Phase:    triggertemplate.PhasePost,
			MaxBytes: 65536,
			Frame:    false,
		})
		require.NoError(t, err)
	})
}

func newTestRuleEvaluator(t *testing.T) (*triggerrule.Rules, *ruleengine.Engine) {
	t.Helper()
	celEng, err := celengine.New(triggerrule.Env(), triggerrule.Stage1Limits(), celengine.DefaultCacheConfig())
	require.NoError(t, err)
	celDialect, err := ruleengine.NewCELDialect(celEng)
	require.NoError(t, err)
	re := ruleengine.NewEngine()
	require.NoError(t, re.Register(celDialect))
	require.NoError(t, re.AllowConsumerDialects(triggerrule.ConsumerName, ruleengine.Grant(ruleengine.DialectCEL)))
	tr, err := triggerrule.New(re)
	require.NoError(t, err)
	return tr, re
}

func evalCELRaw(t *testing.T, re *ruleengine.Engine, expr string, attrs map[string]string, lists map[string][]string) (ruleengine.Decision, error) {
	t.Helper()
	if attrs == nil {
		attrs = map[string]string{}
	}
	if lists == nil {
		lists = map[string][]string{}
	}
	envRef := ruleengine.EnvironmentRef{
		Consumer:     triggerrule.ConsumerName,
		Name:         triggerrule.EnvironmentName,
		MaxRuleBytes: triggerrule.MaxMatchRuleBytes,
	}
	compiled, err := re.Compile(context.Background(), ruleengine.DialectCEL, expr, ruleengine.KindFilter, envRef)
	require.NoError(t, err, "compile CEL %q", expr)
	return re.Evaluate(context.Background(), compiled, ruleengine.Input{
		Activation: map[string]any{
			"event": map[string]any{
				"attributes": attrs,
				"lists":      lists,
			},
			"trigger": map[string]any{
				"id":         "trig-1",
				"name":       "preset-test",
				"project_id": "proj-1",
			},
		},
	}, ruleengine.Budget{})
}

func evalCEL(t *testing.T, re *ruleengine.Engine, expr string, attrs map[string]string, lists map[string][]string) bool {
	t.Helper()
	dec, err := evalCELRaw(t, re, expr, attrs, lists)
	require.NoError(t, err, "evaluate CEL %q", expr)
	return dec.Bool
}

func assertSuggestedActionInvariants(t *testing.T, sa *SuggestedAction, expectBranch bool) {
	t.Helper()
	require.NotNil(t, sa, "expected suggested_action on dispatching preset")
	ensureTestAckCommentSlot(t)

	// Wire-level checks: githubToken="installation", ephemeral=false, and never noAuth or agentRole.
	raw, err := json.Marshal(sa)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(raw, &wire))

	assert.Equal(t, ActionKindDispatchAgent, wire["type"])
	assert.Equal(t, "installation", wire["githubToken"])
	assert.Equal(t, false, wire["ephemeral"])
	assert.NotContains(t, wire, "noAuth", "presets must never set noAuth")
	assert.NotContains(t, wire, "agentRole", "presets must never set agentRole")

	// Template must be literal (never contain "${").
	assert.NotEmpty(t, sa.Template)
	assert.NotContains(t, sa.Template, "${", "template must be literal")

	if expectBranch {
		assert.Equal(t, "${event.attributes.head_ref}", sa.Branch)
		assert.Contains(t, wire, "branch")
	} else {
		assert.Empty(t, sa.Branch)
		assert.NotContains(t, wire, "branch", "fork/issue variants must omit branch")
	}

	// Reserved hub labels must never appear in preset author labels.
	for _, reserved := range []string{"trigger", "trigger_event", "trigger_source"} {
		assert.NotContains(t, sa.Labels, reserved)
	}

	// Validate every template slot and untrustedVars opt-in via pkg/triggertemplate.
	_, err = triggertemplate.ValidateSlot(sa.Task, triggertemplate.SlotTask, sa.UntrustedVars)
	assert.NoError(t, err, "task slot validation failed")

	if sa.AgentName != "" {
		_, err = triggertemplate.ValidateSlot(sa.AgentName, triggertemplate.SlotName, sa.UntrustedVars)
		assert.NoError(t, err, "agentName slot validation failed")
	}
	if sa.Branch != "" {
		_, err = triggertemplate.ValidateSlot(sa.Branch, triggertemplate.SlotBranch, sa.UntrustedVars)
		assert.NoError(t, err, "branch slot validation failed")
	}
	for k, v := range sa.Labels {
		_, err = triggertemplate.ValidateSlot(v, triggertemplate.SlotLabel, sa.UntrustedVars)
		assert.NoError(t, err, "label[%s] slot validation failed", k)
	}
	if sa.ReuseMessage != "" {
		_, err = triggertemplate.ValidateSlot(sa.ReuseMessage, triggertemplate.SlotMessage, sa.UntrustedVars)
		assert.NoError(t, err, "reuseMessage slot validation failed")
	}
	if sa.AckComment != nil {
		_, err = triggertemplate.ValidateSlot(sa.AckComment.Body, testAckCommentSlot, sa.UntrustedVars)
		assert.NoError(t, err, "ackComment.body slot validation failed")
	}
}

func TestPresets_RegistryAndSchemaCompleteness(t *testing.T) {
	all := Presets("")
	byGitHub := Presets(SourceKind)
	assert.Equal(t, all, byGitHub)
	assert.Empty(t, Presets("webhook"))

	wantIDs := []string{
		PresetCommand,
		PresetPROpened,
		PresetPRUpdated,
		PresetIssueOpened,
		PresetLabelAdded,
		PresetReviewSubmitted,
	}
	gotIDs := make([]string, len(all))
	for i, p := range all {
		gotIDs[i] = p.ID
		assert.Equal(t, SourceKind, p.Source)
		assert.Equal(t, PresetVersion, p.Version)
		assert.NotEmpty(t, p.Title)
		assert.NotEmpty(t, p.Description)
		assert.Contains(t, p.Description, `template="default"`, "schema description for %s must document placeholder template", p.ID)
		assert.NotEmpty(t, p.Fields)
		assert.Equal(t, []string{ActionKindDispatchAgent, ActionKindMessage, ActionKindGitHubComment}, p.ActionKinds)

		seenField := make(map[string]bool)
		for _, f := range p.Fields {
			assert.NotEmpty(t, f.Name)
			assert.False(t, seenField[f.Name], "duplicate field %s on %s", f.Name, p.ID)
			seenField[f.Name] = true
			assert.Contains(t, []string{FieldTypeString, FieldTypeEnum, FieldTypeBool, FieldTypeDuration, FieldTypeList}, f.Type)
			assert.NotEmpty(t, f.Help, "missing help text on %s.%s", p.ID, f.Name)
			if f.Type == FieldTypeEnum {
				assert.NotEmpty(t, f.Enum, "enum field %s.%s must list allowed values", p.ID, f.Name)
			}
		}

		lookedUp, ok := LookupPreset(p.ID)
		require.True(t, ok)
		assert.Equal(t, p, lookedUp)
	}
	assert.Equal(t, wantIDs, gotIDs)

	_, ok := LookupPreset("github.nonexistent")
	assert.False(t, ok)

	// Returned slices must be defensive copies.
	all[0].ActionKinds[0] = "mutated"
	all[0].Fields[0].Name = "mutated"
	fresh, ok := LookupPreset(PresetCommand)
	require.True(t, ok)
	assert.Equal(t, ActionKindDispatchAgent, fresh.ActionKinds[0])
	assert.Equal(t, "command", fresh.Fields[0].Name)
}

type presetGoldenCase struct {
	name         string
	req          CompilePresetRequest
	expectBranch bool
}

func presetGoldenCases() []presetGoldenCase {
	return []presetGoldenCase{
		// 1. github.command — defaults & toggles
		{
			name: "command__default",
			req: CompilePresetRequest{
				Preset: PresetCommand,
				Fields: map[string]any{"command": "review"},
			},
			expectBranch: true,
		},
		{
			name: "command__on_issue",
			req: CompilePresetRequest{
				Preset: PresetCommand,
				Fields: map[string]any{
					"command": "plan",
					"on":      "issue",
				},
			},
			expectBranch: false,
		},
		{
			name: "command__on_any_include_body",
			req: CompilePresetRequest{
				Preset: PresetCommand,
				Fields: map[string]any{
					"command":      "implement",
					"on":           "any",
					"include_body": true,
				},
			},
			expectBranch: false,
		},
		{
			name: "command__on_any_include_forks",
			req: CompilePresetRequest{
				Preset: PresetCommand,
				Fields: map[string]any{
					"command":       "triage",
					"on":            "any",
					"include_forks": true,
				},
			},
			expectBranch: false,
		},
		{
			name: "command__allow_bots_include_forks_no_review_comments_custom_assoc",
			req: CompilePresetRequest{
				Preset: PresetCommand,
				Fields: map[string]any{
					"command":                 "my_check-1",
					"on":                      "pull_request",
					"allowed_associations":    []string{"OWNER", "MEMBER"},
					"allow_bots":              true,
					"include_review_comments": false,
					"include_forks":           true,
				},
			},
			expectBranch: false,
		},

		// 2. github.pr_opened — defaults & toggles
		{
			name: "pr_opened__default",
			req: CompilePresetRequest{
				Preset: PresetPROpened,
			},
			expectBranch: true,
		},
		{
			name: "pr_opened__custom_branches_drafts_forks_bots_assoc",
			req: CompilePresetRequest{
				Preset: PresetPROpened,
				Fields: map[string]any{
					"base_branches":        []string{"default", "release/v1.0"},
					"include_drafts":       true,
					"include_forks":        true,
					"allowed_associations": []string{"OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR"},
					"allow_bots":           true,
				},
			},
			expectBranch: false,
		},
		{
			name: "pr_opened__any_base_branch",
			req: CompilePresetRequest{
				Preset: PresetPROpened,
				Fields: map[string]any{
					"base_branches": []string{},
				},
			},
			expectBranch: true,
		},

		// 3. github.pr_updated — defaults & toggles
		{
			name: "pr_updated__default",
			req: CompilePresetRequest{
				Preset: PresetPRUpdated,
			},
			expectBranch: true,
		},
		{
			name: "pr_updated__custom_coalesce_forks_bots",
			req: CompilePresetRequest{
				Preset: PresetPRUpdated,
				Fields: map[string]any{
					"base_branches":   []string{"main"},
					"include_drafts":  true,
					"include_forks":   true,
					"allow_bots":      true,
					"coalesce_window": "2m",
				},
			},
			expectBranch: false,
		},
		{
			name: "pr_updated__coalesce_disabled",
			req: CompilePresetRequest{
				Preset: PresetPRUpdated,
				Fields: map[string]any{
					"coalesce_window": "0s",
				},
			},
			expectBranch: true,
		},

		// 4. github.issue_opened — defaults & toggles
		{
			name: "issue_opened__default",
			req: CompilePresetRequest{
				Preset: PresetIssueOpened,
			},
			expectBranch: false,
		},
		{
			name: "issue_opened__with_labels_bots_assoc",
			req: CompilePresetRequest{
				Preset: PresetIssueOpened,
				Fields: map[string]any{
					"labels_any":           []string{"bug", "triage:urgent"},
					"allowed_associations": []string{"OWNER", "MEMBER", "COLLABORATOR", "FIRST_TIME_CONTRIBUTOR"},
					"allow_bots":           true,
				},
			},
			expectBranch: false,
		},

		// 5. github.label_added — defaults & toggles
		{
			name: "label_added__default",
			req: CompilePresetRequest{
				Preset: PresetLabelAdded,
				Fields: map[string]any{"label": "agent:review"},
			},
			expectBranch: false,
		},
		{
			name: "label_added__on_pr",
			req: CompilePresetRequest{
				Preset: PresetLabelAdded,
				Fields: map[string]any{
					"label": "agent:review",
					"on":    "pull_request",
				},
			},
			expectBranch: true,
		},
		{
			name: "label_added__on_pr_include_forks_allow_bots",
			req: CompilePresetRequest{
				Preset: PresetLabelAdded,
				Fields: map[string]any{
					"label":         "agent:review",
					"on":            "pull_request",
					"include_forks": true,
					"allow_bots":    true,
				},
			},
			expectBranch: false,
		},
		{
			name: "label_added__on_issue",
			req: CompilePresetRequest{
				Preset: PresetLabelAdded,
				Fields: map[string]any{
					"label": "good first issue",
					"on":    "issue",
				},
			},
			expectBranch: false,
		},

		// 6. github.review_submitted — defaults & toggles
		{
			name: "review_submitted__default",
			req: CompilePresetRequest{
				Preset: PresetReviewSubmitted,
			},
			expectBranch: true,
		},
		{
			name: "review_submitted__custom_states_assoc_forks_bots",
			req: CompilePresetRequest{
				Preset: PresetReviewSubmitted,
				Fields: map[string]any{
					"states":               []string{"approved", "commented"},
					"allowed_associations": []string{"OWNER", "MEMBER"},
					"include_forks":        true,
					"allow_bots":           true,
				},
			},
			expectBranch: false,
		},
	}
}

func renderCompiledGolden(t *testing.T, cp *CompiledPreset) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(cp))
	return buf.Bytes()
}

func TestCompilePreset_GoldenAndCELCompilation(t *testing.T) {
	tr, _ := newTestRuleEvaluator(t)
	goldenDir := filepath.Join("testdata", "presets")
	if *update {
		require.NoError(t, os.MkdirAll(goldenDir, 0o755))
	}

	for _, tc := range presetGoldenCases() {
		t.Run(tc.name, func(t *testing.T) {
			out, err := CompilePreset(tc.req)
			require.NoError(t, err)
			require.NotNil(t, out)

			// Determinism: compiling twice yields identical JSON.
			out2, err := CompilePreset(tc.req)
			require.NoError(t, err)
			got := renderCompiledGolden(t, out)
			got2 := renderCompiledGolden(t, out2)
			assert.Equal(t, string(got), string(got2))

			// Form round-trip: re-compiling out.Form produces the exact same output.
			roundTrip, err := CompilePreset(CompilePresetRequest{
				Preset: out.Form.Preset,
				Fields: out.Form.Fields,
			})
			require.NoError(t, err)
			assert.Equal(t, string(got), string(renderCompiledGolden(t, roundTrip)))

			// Every match rule element must be CEL and compile cleanly via pkg/triggerrule (pkg/ruleengine).
			require.NotEmpty(t, out.Match.Types)
			require.Len(t, out.Match.Rules, 1)
			for _, r := range out.Match.Rules {
				assert.Equal(t, DialectCEL, r.Dialect)
				assert.NotEmpty(t, r.Expr)
				err := tr.CompileMatch(context.Background(), triggerrule.Rule{
					Dialect: r.Dialect,
					Expr:    r.Expr,
				})
				require.NoError(t, err, "emitted CEL match rule failed triggerrule.CompileMatch: %s", r.Expr)
			}
			if out.Subject.Expr != "" {
				err := tr.CompileSubject(context.Background(), triggerrule.Rule{
					Dialect: DialectCEL,
					Expr:    out.Subject.Expr,
				})
				require.NoError(t, err, "emitted CEL subject rule failed triggerrule.CompileSubject: %s", out.Subject.Expr)
			}

			// Verify suggested_action invariants & template slots.
			assertSuggestedActionInvariants(t, out.SuggestedAction, tc.expectBranch)

			goldenPath := filepath.Join(goldenDir, tc.name+".golden.json")
			if *update {
				require.NoError(t, os.WriteFile(goldenPath, got, 0o644))
			}
			want, err := os.ReadFile(goldenPath)
			require.NoError(t, err, "missing golden file %s; run with -update", goldenPath)
			assert.Equal(t, string(want), string(got))
		})
	}
}

func TestCompilePreset_RuntimeBehaviorAgainstNormalizedSemantics(t *testing.T) {
	_, re := newTestRuleEvaluator(t)

	t.Run("command preset enforces sender gate, bot exclusion, and fork exclusion", func(t *testing.T) {
		cp, err := CompilePreset(CompilePresetRequest{
			Preset: PresetCommand,
			Fields: map[string]any{"command": "review"},
		})
		require.NoError(t, err)
		expr := cp.Match.Rules[0].Expr

		baseAttrs := map[string]string{
			"command":            "review",
			"is_pull_request":    "true",
			"is_fork":            "false",
			"author_association": "MEMBER",
			"sender_type":        "User",
		}
		assert.True(t, evalCEL(t, re, expr, baseAttrs, nil))

		// NONE commenter rejected
		noneAttrs := cloneMap(baseAttrs)
		noneAttrs["author_association"] = "NONE"
		assert.False(t, evalCEL(t, re, expr, noneAttrs, nil))

		// Bot sender rejected by default
		botAttrs := cloneMap(baseAttrs)
		botAttrs["sender_type"] = "Bot"
		assert.False(t, evalCEL(t, re, expr, botAttrs, nil))

		// Fork PR rejected by default
		forkAttrs := cloneMap(baseAttrs)
		forkAttrs["is_fork"] = "true"
		assert.False(t, evalCEL(t, re, expr, forkAttrs, nil))

		// Un-enriched PR comment (is_fork missing because §5.5 enrichment did not run
		// or failed) fails closed via CEL evaluation error -> skipped fire!
		unenrichedAttrs := cloneMap(baseAttrs)
		delete(unenrichedAttrs, "is_fork")
		_, evalErr := evalCELRaw(t, re, expr, unenrichedAttrs, nil)
		require.Error(t, evalErr)
		var failedErr *ruleengine.FailedError
		require.ErrorAs(t, evalErr, &failedErr)
		reason, isSkip := triggerrule.SkipReason(evalErr)
		assert.True(t, isSkip)
		assert.Equal(t, triggerrule.SkipReasonEvalFailed, reason)
	})

	t.Run("command preset on=any !include_forks short-circuits issues and fails closed on un-enriched PRs", func(t *testing.T) {
		cp, err := CompilePreset(CompilePresetRequest{
			Preset: PresetCommand,
			Fields: map[string]any{
				"command": "implement",
				"on":      "any",
			},
		})
		require.NoError(t, err)
		expr := cp.Match.Rules[0].Expr

		// Plain issue comment (no is_fork attribute) matches cleanly via || short-circuit.
		issueAttrs := map[string]string{
			"command":            "implement",
			"is_pull_request":    "false",
			"author_association": "MEMBER",
			"sender_type":        "User",
		}
		assert.True(t, evalCEL(t, re, expr, issueAttrs, nil))

		// Enriched same-repo PR comment matches.
		sameRepoPR := cloneMap(issueAttrs)
		sameRepoPR["is_pull_request"] = "true"
		sameRepoPR["is_fork"] = "false"
		assert.True(t, evalCEL(t, re, expr, sameRepoPR, nil))

		// Enriched fork PR comment is rejected.
		forkPR := cloneMap(sameRepoPR)
		forkPR["is_fork"] = "true"
		assert.False(t, evalCEL(t, re, expr, forkPR, nil))

		// Un-enriched PR comment (is_pull_request=true, is_fork missing) fails closed!
		unenrichedPR := cloneMap(issueAttrs)
		unenrichedPR["is_pull_request"] = "true"
		_, evalErr := evalCELRaw(t, re, expr, unenrichedPR, nil)
		require.Error(t, evalErr)
		reason, isSkip := triggerrule.SkipReason(evalErr)
		assert.True(t, isSkip)
		assert.Equal(t, triggerrule.SkipReasonEvalFailed, reason)
	})

	t.Run("issue_opened uses event.lists.labels", func(t *testing.T) {
		cp, err := CompilePreset(CompilePresetRequest{
			Preset: PresetIssueOpened,
			Fields: map[string]any{"labels_any": []string{"bug", "security"}},
		})
		require.NoError(t, err)
		expr := cp.Match.Rules[0].Expr

		attrs := map[string]string{
			"author_association": "OWNER",
			"sender_type":        "User",
		}
		assert.True(t, evalCEL(t, re, expr, attrs, map[string][]string{"labels": {"docs", "bug"}}))
		assert.False(t, evalCEL(t, re, expr, attrs, map[string][]string{"labels": {"docs"}}))
		assert.False(t, evalCEL(t, re, expr, attrs, map[string][]string{"labels": {}}))
	})
}

func TestCompilePreset_HostileInputsAndCELInjection(t *testing.T) {
	tr, re := newTestRuleEvaluator(t)

	t.Run("hostile values rejected with content-free field errors", func(t *testing.T) {
		cases := []struct {
			name      string
			req       CompilePresetRequest
			wantField string
		}{
			{
				name:      "empty preset",
				req:       CompilePresetRequest{},
				wantField: "preset",
			},
			{
				name:      "unknown preset with injection",
				req:       CompilePresetRequest{Preset: "github.command' || true"},
				wantField: "preset",
			},
			{
				name: "command with single quote and OR",
				req: CompilePresetRequest{
					Preset: PresetCommand,
					Fields: map[string]any{"command": "review' || true || 'a"},
				},
				wantField: "command",
			},
			{
				name: "command with leading slash",
				req: CompilePresetRequest{
					Preset: PresetCommand,
					Fields: map[string]any{"command": "/review"},
				},
				wantField: "command",
			},
			{
				name: "command with uppercase",
				req: CompilePresetRequest{
					Preset: PresetCommand,
					Fields: map[string]any{"command": "Review"},
				},
				wantField: "command",
			},
			{
				name: "command with newline",
				req: CompilePresetRequest{
					Preset: PresetCommand,
					Fields: map[string]any{"command": "review\n|| true"},
				},
				wantField: "command",
			},
			{
				name: "command overlong (>32 chars)",
				req: CompilePresetRequest{
					Preset: PresetCommand,
					Fields: map[string]any{"command": strings.Repeat("a", 33)},
				},
				wantField: "command",
			},
			{
				name: "base_branches with quote injection",
				req: CompilePresetRequest{
					Preset: PresetPROpened,
					Fields: map[string]any{"base_branches": []string{"main' || true || '"}},
				},
				wantField: "base_branches",
			},
			{
				name: "base_branches with backslash",
				req: CompilePresetRequest{
					Preset: PresetPROpened,
					Fields: map[string]any{"base_branches": []string{`main\`}},
				},
				wantField: "base_branches",
			},
			{
				name: "base_branches with dot-dot",
				req: CompilePresetRequest{
					Preset: PresetPROpened,
					Fields: map[string]any{"base_branches": []string{"../etc"}},
				},
				wantField: "base_branches",
			},
			{
				name: "base_branches overlong",
				req: CompilePresetRequest{
					Preset: PresetPROpened,
					Fields: map[string]any{"base_branches": []string{strings.Repeat("b", 201)}},
				},
				wantField: "base_branches",
			},
			{
				name: "allowed_associations injection",
				req: CompilePresetRequest{
					Preset: PresetPROpened,
					Fields: map[string]any{"allowed_associations": []string{"OWNER' || true || '"}},
				},
				wantField: "allowed_associations",
			},
			{
				name: "allowed_associations empty list",
				req: CompilePresetRequest{
					Preset: PresetPROpened,
					Fields: map[string]any{"allowed_associations": []string{}},
				},
				wantField: "allowed_associations",
			},
			{
				name: "label with newline",
				req: CompilePresetRequest{
					Preset: PresetLabelAdded,
					Fields: map[string]any{"label": "bug\n' || true"},
				},
				wantField: "label",
			},
			{
				name: "label with unicode bidi format char",
				req: CompilePresetRequest{
					Preset: PresetLabelAdded,
					Fields: map[string]any{"label": "bug\u202eevil"},
				},
				wantField: "label",
			},
			{
				name: "label overlong",
				req: CompilePresetRequest{
					Preset: PresetLabelAdded,
					Fields: map[string]any{"label": strings.Repeat("x", 101)},
				},
				wantField: "label",
			},
			{
				name: "labels_any with NUL byte",
				req: CompilePresetRequest{
					Preset: PresetIssueOpened,
					Fields: map[string]any{"labels_any": []string{"ok", "bad\x00label"}},
				},
				wantField: "labels_any",
			},
			{
				name: "review_submitted invalid state dismissed",
				req: CompilePresetRequest{
					Preset: PresetReviewSubmitted,
					Fields: map[string]any{"states": []string{"dismissed"}},
				},
				wantField: "states",
			},
			{
				name: "coalesce_window injection",
				req: CompilePresetRequest{
					Preset: PresetPRUpdated,
					Fields: map[string]any{"coalesce_window": "30s' || true"},
				},
				wantField: "coalesce_window",
			},
			{
				name: "coalesce_window exceeds 10m",
				req: CompilePresetRequest{
					Preset: PresetPRUpdated,
					Fields: map[string]any{"coalesce_window": "11m"},
				},
				wantField: "coalesce_window",
			},
			{
				name: "coalesce_window negative",
				req: CompilePresetRequest{
					Preset: PresetPRUpdated,
					Fields: map[string]any{"coalesce_window": "-5s"},
				},
				wantField: "coalesce_window",
			},
			{
				name: "unknown field rejected",
				req: CompilePresetRequest{
					Preset: PresetPROpened,
					Fields: map[string]any{"extra_field": "evil_payload_value"},
				},
				wantField: "extra_field",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out, err := CompilePreset(tc.req)
				require.Error(t, err)
				assert.Nil(t, out)

				var cErr *PresetCompileError
				require.True(t, errors.As(err, &cErr), "expected *PresetCompileError, got %T: %v", err, err)
				require.NotEmpty(t, cErr.Fields)

				var gotFields []string
				for _, fe := range cErr.Fields {
					gotFields = append(gotFields, fe.Field)
					assert.NotEmpty(t, fe.Code)
					assert.NotEmpty(t, fe.Message)
					// Error messages must be content-free (never echo injected tokens).
					assert.NotContains(t, fe.Message, "|| true")
					assert.NotContains(t, fe.Message, "evil_payload_value")
				}
				assert.Contains(t, gotFields, tc.wantField)
				assert.NotContains(t, cErr.Error(), "|| true")
				assert.NotContains(t, cErr.Error(), "evil_payload_value")
			})
		}
	})

	t.Run("printable hostile label characters are safely quoted and cannot inject CEL", func(t *testing.T) {
		hostileLabels := []string{
			`won't fix`,
			`a' || true || 'b`,
			`a" || true || "b`,
			`trailing-backslash\`,
			`quote-after-slash\' || true || 'x`,
			`${event.attributes.sender}`,
			`backticks` + "`" + ` || true`,
		}

		for _, hostile := range hostileLabels {
			cp, err := CompilePreset(CompilePresetRequest{
				Preset: PresetLabelAdded,
				Fields: map[string]any{"label": hostile},
			})
			require.NoError(t, err, "label %q should be safely quoted", hostile)
			expr := cp.Match.Rules[0].Expr

			// Must compile under triggerrule.
			require.NoError(t, tr.CompileMatch(context.Background(), triggerrule.Rule{
				Dialect: DialectCEL,
				Expr:    expr,
			}))

			// Semantic proof of zero CEL injection: an event with a DIFFERENT label
			// MUST evaluate to false even when hostile contains `' || true || '`!
			otherAttrs := map[string]string{
				"label":           "completely-different",
				"is_pull_request": "false",
				"sender_type":     "User",
			}
			assert.False(t, evalCEL(t, re, expr, otherAttrs, nil),
				"CEL injection detected for label %q in expr %s", hostile, expr)

			// And an event with the EXACT label evaluates to true!
			exactAttrs := map[string]string{
				"label":           hostile,
				"is_pull_request": "false",
				"sender_type":     "User",
			}
			assert.True(t, evalCEL(t, re, expr, exactAttrs, nil),
				"exact label %q failed to match in expr %s", hostile, expr)
		}
	})
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
