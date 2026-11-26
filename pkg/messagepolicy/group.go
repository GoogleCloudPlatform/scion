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

package messagepolicy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/GoogleCloudPlatform/scion/pkg/ruleengine"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/spec"
)

// GroupKeyConfig supplies the consumer, authority-reference, and evaluation
// option inputs that form part of a System One evaluation group's identity.
type GroupKeyConfig struct {
	// Consumer defaults to ConsumerName ("message_policy") when empty.
	Consumer string
	// Dialect defaults to systemone.DialectName ("system_one") when empty.
	Dialect string
	// AllowlistRef identifies the project allowlist reference governing
	// binding resolution; defaults to req.ProjectID when empty.
	AllowlistRef string
	// MaxEvalTime is the per-evaluation deadline ceiling passed in soOpts.
	MaxEvalTime time.Duration
	// Budget is the optional aggregate BudgetTracker passed in soOpts.
	Budget *ruleengine.BudgetTracker
}

type groupKeyFieldEntry struct {
	Name    string                `json:"name"`
	Present bool                  `json:"present"`
	Fields  []provider.InputField `json:"fields,omitempty"`
}

type groupKeyStringSlice struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

type groupKeyStringPair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type canonicalGroupKeyPayload struct {
	AllowlistRef    string                `json:"allowlist_ref"`
	BindingInstance string                `json:"binding_instance"`
	BindingPool     string                `json:"binding_pool"`
	Budget          string                `json:"budget"`
	ChoiceSets      []groupKeyStringSlice `json:"choice_sets,omitempty"`
	Consumer        string                `json:"consumer"`
	ContentHash     string                `json:"content_hash"`
	Dialect         string                `json:"dialect"`
	EnvName         string                `json:"env_name"`
	EnvVersion      int                   `json:"env_version"`
	MaxEvalTimeNs   int64                 `json:"max_eval_time_ns"`
	Params          []groupKeyStringPair  `json:"params,omitempty"`
	ProjectID       string                `json:"project_id"`
	RuleTimeoutNs   int64                 `json:"rule_timeout_ns"`
	SelectedInputs  []groupKeyFieldEntry  `json:"selected_inputs"`
	SpecVersion     int                   `json:"spec_version"`
}

// GroupKey computes a canonical, deterministic, content-free SHA-256 identity
// key for a planned System One evaluation group.
//
// Policy ID (req.RuleID), revision (req.Revision), and ElementIndex are
// intentionally excluded so distinct policies sharing an identical judgment,
// authority, and evaluation configuration coalesce into one planned execution.
// Returns "" when compiled is nil.
func GroupKey(compiled *spec.Compiled, req systemone.Request, cfg GroupKeyConfig) string {
	if compiled == nil {
		return ""
	}
	consumer := strings.TrimSpace(cfg.Consumer)
	if consumer == "" {
		consumer = ConsumerName
	}
	dialect := strings.TrimSpace(cfg.Dialect)
	if dialect == "" {
		dialect = systemone.DialectName
	}
	projectID := strings.TrimSpace(req.ProjectID)
	allowlistRef := strings.TrimSpace(cfg.AllowlistRef)
	if allowlistRef == "" {
		allowlistRef = projectID
	}

	budgetKey := "none"
	if cfg.Budget != nil {
		budgetKey = fmt.Sprintf("ptr:%p", cfg.Budget)
	}

	byName := make(map[string][]provider.InputField, len(req.Input.Fields))
	for _, f := range req.Input.Fields {
		byName[f.Name] = append(byName[f.Name], f)
	}

	selectedNames := slices.Clone(compiled.Inputs)
	sort.Strings(selectedNames)
	selectedInputs := make([]groupKeyFieldEntry, 0, len(selectedNames))
	for _, name := range selectedNames {
		fields, ok := byName[name]
		selectedInputs = append(selectedInputs, groupKeyFieldEntry{
			Name:    name,
			Present: ok,
			Fields:  fields,
		})
	}

	var choiceSets []groupKeyStringSlice
	if len(req.Input.ChoiceSets) > 0 {
		keys := make([]string, 0, len(req.Input.ChoiceSets))
		for k := range req.Input.ChoiceSets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		choiceSets = make([]groupKeyStringSlice, 0, len(keys))
		for _, k := range keys {
			choiceSets = append(choiceSets, groupKeyStringSlice{
				Key:    k,
				Values: req.Input.ChoiceSets[k],
			})
		}
	}

	var params []groupKeyStringPair
	if len(req.Input.Params) > 0 {
		keys := make([]string, 0, len(req.Input.Params))
		for k := range req.Input.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		params = make([]groupKeyStringPair, 0, len(keys))
		for _, k := range keys {
			params = append(params, groupKeyStringPair{
				Key:   k,
				Value: req.Input.Params[k],
			})
		}
	}

	payload := canonicalGroupKeyPayload{
		AllowlistRef:    allowlistRef,
		BindingInstance: compiled.Spec.Binding.Instance,
		BindingPool:     compiled.Spec.Binding.Pool,
		Budget:          budgetKey,
		ChoiceSets:      choiceSets,
		Consumer:        consumer,
		ContentHash:     compiled.ContentHash,
		Dialect:         dialect,
		EnvName:         compiled.Environment.Name,
		EnvVersion:      compiled.Environment.Version,
		MaxEvalTimeNs:   int64(cfg.MaxEvalTime),
		Params:          params,
		ProjectID:       projectID,
		RuleTimeoutNs:   int64(compiled.Timeout),
		SelectedInputs:  selectedInputs,
		SpecVersion:     compiled.Spec.Version,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return ""
	}
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return "s1group:v1:" + hex.EncodeToString(sum[:])
}

// GroupCandidate represents one eligible System One policy unit submitted to
// the immutable per-send evaluation group planner.
type GroupCandidate struct {
	Index         int
	PolicyID      string
	PolicyName    string
	Revision      int64
	SystemOneRule string
}

// EvaluationGroup is an immutable planned group of policy units that share one
// System One judgment execution under the caller's execution context.
type EvaluationGroup struct {
	Key      string
	Compiled ruleengine.Compiled
	Primary  GroupCandidate
	Members  []GroupCandidate
}

// UngroupedFailure records a candidate whose System One rule failed
// compilation (or encountered dialect_disabled) and is therefore never grouped.
type UngroupedFailure struct {
	Candidate      GroupCandidate
	Classification Classification
}

// GroupPlan is the immutable partition of eligible System One candidates
// planned before any System One work is launched.
type GroupPlan struct {
	Groups    []EvaluationGroup
	Ungrouped []UngroupedFailure
}

// PlanSystemOneGroups compiles each eligible System One candidate and
// partitions successfully compiled candidates into immutable evaluation groups
// keyed by GroupKey. Compile failures and dialect_disabled results are never
// grouped and are returned in GroupPlan.Ungrouped with their per-policy
// Classification.
//
// Each group's Primary is selected deterministically by lowest PolicyName,
// then lowest PolicyID (and lowest Index if both tie).
func (e *Evaluator) PlanSystemOneGroups(
	ctx context.Context,
	candidates []GroupCandidate,
	req systemone.Request,
	cfg GroupKeyConfig,
) GroupPlan {
	var plan GroupPlan
	if len(candidates) == 0 {
		return plan
	}

	contentEnv := SystemOneContentEnv()
	groupIdxByKey := make(map[string]int, len(candidates))

	for _, cand := range candidates {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				plan.Ungrouped = append(plan.Ungrouped, UngroupedFailure{
					Candidate:      cand,
					Classification: ClassifyError(&ruleengine.FailedError{Reason: celengine.ReasonCanceled, Cause: err}),
				})
				continue
			}
		}
		compiled, err := e.CompileSystemOne(ctx, cand.SystemOneRule)
		if err != nil {
			plan.Ungrouped = append(plan.Ungrouped, UngroupedFailure{
				Candidate:      cand,
				Classification: e.classifySystemOneCompileError(err),
			})
			continue
		}
		specCompiled, err := spec.Compile([]byte(strings.TrimSpace(cand.SystemOneRule)), spec.CompileOptions{
			Kind:        spec.Kind(KindJudgments),
			Environment: &contentEnv,
		})
		if err != nil {
			plan.Ungrouped = append(plan.Ungrouped, UngroupedFailure{
				Candidate:      cand,
				Classification: ClassifyError(err),
			})
			continue
		}
		key := GroupKey(specCompiled, req, cfg)
		if key == "" {
			plan.Ungrouped = append(plan.Ungrouped, UngroupedFailure{
				Candidate:      cand,
				Classification: ClassifyError(&ruleengine.FailedError{Reason: celengine.ReasonRuntime}),
			})
			continue
		}
		if gi, ok := groupIdxByKey[key]; ok {
			plan.Groups[gi].Members = append(plan.Groups[gi].Members, cand)
			if candidateLess(cand, plan.Groups[gi].Primary) {
				plan.Groups[gi].Primary = cand
			}
		} else {
			groupIdxByKey[key] = len(plan.Groups)
			plan.Groups = append(plan.Groups, EvaluationGroup{
				Key:      key,
				Compiled: compiled,
				Primary:  cand,
				Members:  []GroupCandidate{cand},
			})
		}
	}

	return plan
}

// candidateLess defines deterministic primary selection: lowest PolicyName,
// then lowest PolicyID, then lowest Index.
func candidateLess(a, b GroupCandidate) bool {
	if a.PolicyName != b.PolicyName {
		return a.PolicyName < b.PolicyName
	}
	if a.PolicyID != b.PolicyID {
		return a.PolicyID < b.PolicyID
	}
	return a.Index < b.Index
}

// CloneEvaluationForSecondary deep-copies a primary member's
// *systemone.Evaluation for a non-primary member of the same planned group:
//   - assigns a fresh unique EvalID;
//   - sets Rule.RuleID and Rule.Revision to the secondary member's policy ref;
//   - sets CacheHit = true;
//   - clears Usage (only the primary member carries provider usage).
//
// Returns nil when primary is nil.
func CloneEvaluationForSecondary(primary *systemone.Evaluation, policyID string, revision int64) *systemone.Evaluation {
	if primary == nil {
		return nil
	}
	cp := *primary
	cp.EvalID = newGroupMemberEvalID()
	cp.Rule.RuleID = policyID
	cp.Rule.Revision = revision
	if primary.Rule.ElementIndex != nil {
		idx := *primary.Rule.ElementIndex
		cp.Rule.ElementIndex = &idx
	}
	cp.CacheHit = true
	cp.Usage = nil

	if primary.Auth != nil {
		authCp := *primary.Auth
		if primary.Auth.SecretRef != nil {
			sr := *primary.Auth.SecretRef
			authCp.SecretRef = &sr
		}
		if primary.Auth.Scopes != nil {
			authCp.Scopes = slices.Clone(primary.Auth.Scopes)
		}
		cp.Auth = &authCp
	}

	if primary.Answers != nil {
		cp.Answers = make(map[string]systemone.AnswerRecord, len(primary.Answers))
		for k, ans := range primary.Answers {
			ansCp := ans
			if ans.Bool != nil {
				v := *ans.Bool
				ansCp.Bool = &v
			}
			if ans.Choice != nil {
				v := *ans.Choice
				ansCp.Choice = &v
			}
			if ans.Level != nil {
				v := *ans.Level
				ansCp.Level = &v
			}
			if ans.ExpectedIndex != nil {
				v := *ans.ExpectedIndex
				ansCp.ExpectedIndex = &v
			}
			if ans.Distribution != nil {
				ansCp.Distribution = make(map[string]float64, len(ans.Distribution))
				maps.Copy(ansCp.Distribution, ans.Distribution)
			}
			if ans.Metrics != nil {
				ansCp.Metrics = maps.Clone(ans.Metrics)
			}
			if ans.NativeMetrics != nil {
				ansCp.NativeMetrics = maps.Clone(ans.NativeMetrics)
			}
			if ans.Notes != nil {
				ansCp.Notes = slices.Clone(ans.Notes)
			}
			cp.Answers[k] = ansCp
		}
	}

	if primary.Uncertainty != nil {
		cp.Uncertainty = slices.Clone(primary.Uncertainty)
	}
	if primary.Notes != nil {
		cp.Notes = slices.Clone(primary.Notes)
	}
	if primary.Attempts != nil {
		cp.Attempts = slices.Clone(primary.Attempts)
	}
	if primary.Input.Fields != nil {
		cp.Input.Fields = slices.Clone(primary.Input.Fields)
	}

	return &cp
}

func newGroupMemberEvalID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("eval-group-%d", time.Now().UnixNano())
	}
	return "eval-" + hex.EncodeToString(b[:])
}
