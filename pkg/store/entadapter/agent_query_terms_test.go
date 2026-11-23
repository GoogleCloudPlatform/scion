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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/GoogleCloudPlatform/scion/pkg/filterquery"
	"github.com/GoogleCloudPlatform/scion/pkg/filterquery/schemas"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func queryToStoreTerms(q filterquery.Query) *store.QueryTerms {
	if q.IsEmpty() {
		return nil
	}
	qt := &store.QueryTerms{
		Text:        q.Text,
		ExcludeText: q.ExcludeText,
	}
	for _, t := range q.Terms {
		op := store.QueryOpEq
		if t.Key == "label" {
			allHasKey := true
			allEq := true
			for _, v := range t.Values {
				if strings.Contains(v, "=") {
					allHasKey = false
				} else {
					allEq = false
				}
			}
			if allHasKey {
				op = store.QueryOpHasKey
			} else if allEq {
				op = store.QueryOpLabelEq
			}
		}
		vals := make([]string, len(t.Values))
		for i, v := range t.Values {
			if t.Key == "phase" {
				vals[i] = strings.ToLower(v)
			} else {
				vals[i] = v
			}
		}
		qt.Terms = append(qt.Terms, store.QueryTerm{
			Field:  t.Key,
			Values: vals,
			Negate: t.Negate,
			Op:     op,
		})
	}
	return qt
}

func TestListAgents_QueryTerms_ConformanceParity(t *testing.T) {
	as, projID := newTestAgentStore(t)
	ctx := context.Background()

	raw, err := os.ReadFile(filepath.Join("..", "..", "filterquery", "testdata", "conformance.json"))
	if err != nil {
		t.Fatalf("read conformance.json: %v", err)
	}
	var cf struct {
		Eval struct {
			Rows []struct {
				ID     string            `json:"id"`
				Name   string            `json:"name"`
				Slug   string            `json:"slug"`
				Phase  string            `json:"phase"`
				Labels map[string]string `json:"labels"`
			} `json:"rows"`
			Cases []struct {
				Name      string   `json:"name"`
				Query     string   `json:"query"`
				ExpectIDs []string `json:"expectIds"`
			} `json:"cases"`
		} `json:"eval"`
	}
	if err := json.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("unmarshal conformance.json: %v", err)
	}

	idToLogical := make(map[string]string, len(cf.Eval.Rows))
	for _, r := range cf.Eval.Rows {
		uid := uuid.NewString()
		idToLogical[uid] = r.ID
		var labels map[string]string
		if len(r.Labels) > 0 {
			labels = r.Labels
		}
		if err := as.CreateAgent(ctx, &store.Agent{
			ID:        uid,
			Name:      r.Name,
			Slug:      r.Slug,
			ProjectID: projID,
			Phase:     r.Phase,
			Labels:    labels,
		}); err != nil {
			t.Fatalf("CreateAgent(%s): %v", r.ID, err)
		}
	}

	for _, tc := range cf.Eval.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			q, fqErr := filterquery.Parse(tc.Query, schemas.Agents.HasKey)
			if fqErr != nil {
				t.Fatalf("Parse(%q): %v", tc.Query, fqErr)
			}
			res, err := as.ListAgents(ctx, store.AgentFilter{
				ProjectID: projID,
				Query:     queryToStoreTerms(q),
			}, store.ListOptions{Limit: 100})
			if err != nil {
				t.Fatalf("ListAgents(%q): %v", tc.Query, err)
			}
			got := make([]string, 0, len(res.Items))
			for _, item := range res.Items {
				got = append(got, idToLogical[item.ID])
			}
			slices.Sort(got)
			want := append([]string(nil), tc.ExpectIDs...)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("ListAgents(%q) = %v, want %v", tc.Query, got, want)
			}
			if res.TotalCount != len(want) {
				t.Errorf("TotalCount(%q) = %d, want %d", tc.Query, res.TotalCount, len(want))
			}
		})
	}
}

func TestListAgents_QueryTerms_AuthzIntersectionAndUnknownField(t *testing.T) {
	as, p1ID := newTestAgentStore(t)
	ctx := context.Background()

	p2UID := uuid.New()
	if _, err := as.client.Project.Create().SetID(p2UID).SetName("authz-p2").SetSlug("authz-p2").Save(ctx); err != nil {
		t.Fatalf("CreateProject p2: %v", err)
	}

	a1 := &store.Agent{ID: uuid.NewString(), Name: "alpha-1", Slug: "alpha-1", ProjectID: p1ID, Phase: "running"}
	a2 := &store.Agent{ID: uuid.NewString(), Name: "alpha-2", Slug: "alpha-2", ProjectID: p2UID.String(), Phase: "running"}
	if err := as.CreateAgent(ctx, a1); err != nil {
		t.Fatalf("CreateAgent a1: %v", err)
	}
	if err := as.CreateAgent(ctx, a2); err != nil {
		t.Fatalf("CreateAgent a2: %v", err)
	}

	// Query matching both agents must still be restricted to AuthorizedProjectIDs.
	res, err := as.ListAgents(ctx, store.AgentFilter{
		AuthorizedProjectIDs: []string{p1ID},
		Query: &store.QueryTerms{
			Terms: []store.QueryTerm{
				{Field: "phase", Values: []string{"running"}, Op: store.QueryOpEq},
			},
			Text: "alpha",
		},
	}, store.ListOptions{Limit: 100})
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != a1.ID {
		t.Fatalf("got %d items, want only a1 (%s)", len(res.Items), a1.ID)
	}

	// Unknown field must return ErrInvalidInput, never a no-op.
	_, err = as.ListAgents(ctx, store.AgentFilter{
		Query: &store.QueryTerms{
			Terms: []store.QueryTerm{
				{Field: "unknown_field", Values: []string{"x"}, Op: store.QueryOpEq},
			},
		},
	}, store.ListOptions{Limit: 100})
	if !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("unknown field err = %v, want store.ErrInvalidInput", err)
	}
}

func TestListAgents_QueryTerms_BackslashLabelKeys(t *testing.T) {
	as, projID := newTestAgentStore(t)
	ctx := context.Background()

	withBackslash := &store.Agent{
		ID:        uuid.NewString(),
		Name:      "with-bs",
		Slug:      "with-bs",
		ProjectID: projID,
		Phase:     "running",
		Labels:    map[string]string{`foo\`: "1", `k\`: "v"},
	}
	plain := &store.Agent{
		ID:        uuid.NewString(),
		Name:      "plain",
		Slug:      "plain",
		ProjectID: projID,
		Phase:     "running",
	}
	if err := as.CreateAgent(ctx, withBackslash); err != nil {
		t.Fatalf("CreateAgent withBackslash: %v", err)
	}
	if err := as.CreateAgent(ctx, plain); err != nil {
		t.Fatalf("CreateAgent plain: %v", err)
	}

	cases := []struct {
		name   string
		term   store.QueryTerm
		wantID string
	}{
		{
			name:   "has-key trailing backslash",
			term:   store.QueryTerm{Field: "label", Values: []string{`foo\`}, Op: store.QueryOpHasKey},
			wantID: withBackslash.ID,
		},
		{
			name:   "negated has-key trailing backslash",
			term:   store.QueryTerm{Field: "label", Values: []string{`foo\`}, Negate: true, Op: store.QueryOpHasKey},
			wantID: plain.ID,
		},
		{
			name:   "label eq trailing backslash",
			term:   store.QueryTerm{Field: "label", Values: []string{`k\=v`}, Op: store.QueryOpLabelEq},
			wantID: withBackslash.ID,
		},
		{
			name:   "negated label eq trailing backslash",
			term:   store.QueryTerm{Field: "label", Values: []string{`k\=v`}, Negate: true, Op: store.QueryOpLabelEq},
			wantID: plain.ID,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := as.ListAgents(ctx, store.AgentFilter{
				ProjectID: projID,
				Query:     &store.QueryTerms{Terms: []store.QueryTerm{tc.term}},
			}, store.ListOptions{Limit: 10})
			if err != nil {
				t.Fatalf("ListAgents(%s): %v", tc.name, err)
			}
			if len(res.Items) != 1 || res.Items[0].ID != tc.wantID {
				t.Fatalf("ListAgents(%s) got %d items, want [%s]", tc.name, len(res.Items), tc.wantID)
			}
		})
	}
}

func TestPostgresLabelJSONOperand_ControlCharacters(t *testing.T) {
	cases := []struct {
		key, val string
	}{
		{key: "k\x00null", val: "v\x01soh"},
		{key: "k\nline", val: "v\t\"quote\"\\"},
		{key: `foo\`, val: "bar"},
	}
	for _, tc := range cases {
		raw := postgresLabelJSONOperand(tc.key, tc.val)
		var decoded map[string]string
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("postgresLabelJSONOperand(%q, %q) = %s is not valid JSON: %v", tc.key, tc.val, raw, err)
		}
		if len(decoded) != 1 || decoded[tc.key] != tc.val {
			t.Fatalf("decoded = %#v, want map[%q:%q]", decoded, tc.key, tc.val)
		}
	}
}
