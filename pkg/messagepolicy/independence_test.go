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

package messagepolicy_test

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/celengine"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIndependence_CheckedAST(t *testing.T) {
	ev, _ := newTestEvaluatorWithSystemOne(t, nil, nil)

	ctx := context.Background()

	s1BodyOnly := `{"dialect":"system_one","inputs":["message.body"],"spec":{"version":1,"questions":[{"id":"q","type":"binary","instructions":"is bad?"}]}}`
	s1RecipientSlug := `{"dialect":"system_one","inputs":["message.body","recipient.slug"],"spec":{"version":1,"questions":[{"id":"q","type":"binary","instructions":"is bad?"}]}}`
	s1Unmentioned := `{"dialect":"system_one","inputs":["message.body","conversation.unmentioned_agent_slugs"],"spec":{"version":1,"questions":[{"id":"q","type":"binary","instructions":"is bad?"}]}}`
	s1Invalid := `{not valid json`

	tests := []struct {
		name            string
		violationRule   string
		systemOneRule   string
		wantIndependent bool
		wantErr         bool
	}{
		{
			name:            "body only is independent",
			violationRule:   `message.body.contains("secret")`,
			wantIndependent: true,
		},
		{
			name:            "string literal containing recipient is independent (kills lexical-only detection)",
			violationRule:   `message.body.contains("recipient.id") && message.body != "conversation"`,
			wantIndependent: true,
		},
		{
			name:            "surface and sender only is independent",
			violationRule:   `surface == "mention_delivery" && sender.slug == "alice"`,
			wantIndependent: true,
		},
		{
			name:            "ident form referencing recipient map size",
			violationRule:   `size(recipient) > 0`,
			wantIndependent: false,
		},
		{
			name:            "select form referencing recipient.slug",
			violationRule:   `recipient.slug == "bob"`,
			wantIndependent: false,
		},
		{
			name:            "index form referencing recipient",
			violationRule:   `recipient["id"] == "agent-1"`,
			wantIndependent: false,
		},
		{
			name:            "has macro referencing recipient",
			violationRule:   `has(recipient.slug)`,
			wantIndependent: false,
		},
		{
			name:            "comprehension macro referencing conversation participants",
			violationRule:   `conversation.agent_participant_ids.exists(p, p == sender.id)`,
			wantIndependent: false,
		},
		{
			name:            "comprehension body referencing recipient inside exists",
			violationRule:   `mentioned_agent_ids.exists(m, m == recipient.id)`,
			wantIndependent: false,
		},
		{
			name:            "shadowed comprehension variable over-approximates as dependent",
			violationRule:   `mentioned_agent_ids.exists(recipient, recipient == "x")`,
			wantIndependent: false,
		},
		{
			name:            "unknown identifier fails compile and is conservatively dependent",
			violationRule:   `unknown_var == "x"`,
			wantIndependent: false,
			wantErr:         true,
		},
		{
			name:            "parse error fails compile and is conservatively dependent",
			violationRule:   `message.body == (`,
			wantIndependent: false,
			wantErr:         true,
		},
		{
			name:            "system one body-only with independent CEL is independent",
			violationRule:   `judgment_flags["q"]`,
			systemOneRule:   s1BodyOnly,
			wantIndependent: true,
		},
		{
			name:            "system one recipient.slug makes policy dependent",
			violationRule:   `judgment_flags["q"]`,
			systemOneRule:   s1RecipientSlug,
			wantIndependent: false,
		},
		{
			name:            "system one unmentioned_agent_slugs makes policy dependent",
			violationRule:   `judgment_flags["q"]`,
			systemOneRule:   s1Unmentioned,
			wantIndependent: false,
		},
		{
			name:            "system one compile error is conservatively dependent",
			violationRule:   `judgment_flags["q"]`,
			systemOneRule:   s1Invalid,
			wantIndependent: false,
			wantErr:         true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			indep, err := ev.IsRecipientIndependent(ctx, tc.violationRule, tc.systemOneRule)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.wantIndependent, indep)
		})
	}

	t.Run("zero celengine.Program has no checked AST and fails conservatively", func(t *testing.T) {
		var zero celengine.Program
		refs, err := zero.ReferencesIdent("recipient")
		require.Error(t, err)
		assert.True(t, refs)
	})
}
