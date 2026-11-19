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

package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/messagepolicy"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessagePolicies_CreateListGetUpdateDelete(t *testing.T) {
	now := time.Date(2026, 11, 17, 6, 0, 0, 0, time.UTC)
	cov := messagepolicy.Coverage()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/message-policies":
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			var req CreateMessagePolicyRequest
			require.NoError(t, dec.Decode(&req))
			assert.Equal(t, "no-prose-choices", req.Name)
			assert.Equal(t, "enforce", req.Mode)
			assert.Equal(t, []string{"agent_to_user"}, req.Scope.Surfaces)
			assert.Equal(t, "!message.has_choices && message.body.contains('Option A')", req.ViolationRule)
			assert.Equal(t, "reject", req.OnFailed)

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(MessagePolicyResponse{
				MessagePolicy: store.MessagePolicy{
					ID:                "pol-1",
					ProjectID:         "proj-1",
					Name:              req.Name,
					Enabled:           true,
					Mode:              req.Mode,
					Scope:             req.Scope,
					ViolationRule:     req.ViolationRule,
					OnFailed:          req.OnFailed,
					RejectReason:      req.RejectReason,
					RejectRemediation: req.RejectRemediation,
					Revision:          1,
					CreatedAt:         now,
					UpdatedAt:         now,
				},
				Coverage: cov,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/message-policies":
			assert.Equal(t, "enforce", r.URL.Query().Get("mode"))
			assert.Equal(t, "true", r.URL.Query().Get("enabled"))
			assert.Equal(t, "25", r.URL.Query().Get("limit"))
			assert.Equal(t, "cur-1", r.URL.Query().Get("cursor"))

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ListMessagePoliciesResponse{
				Policies: []MessagePolicyResponse{
					{
						MessagePolicy: store.MessagePolicy{
							ID:        "pol-1",
							ProjectID: "proj-1",
							Name:      "no-prose-choices",
							Enabled:   true,
							Mode:      "enforce",
							Scope:     store.MessagePolicyScope{Surfaces: []string{"agent_to_user"}},
							Revision:  1,
						},
						Coverage: cov,
					},
				},
				Coverage:   cov,
				NextCursor: "cur-2",
				TotalCount: 1,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/message-policies/no-prose-choices":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(MessagePolicyResponse{
				MessagePolicy: store.MessagePolicy{
					ID:        "pol-1",
					ProjectID: "proj-1",
					Name:      "no-prose-choices",
					Enabled:   true,
					Mode:      "enforce",
					Scope:     store.MessagePolicyScope{Surfaces: []string{"agent_to_user"}},
					Revision:  1,
				},
				Coverage: cov,
			})

		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/projects/proj-1/message-policies/no-prose-choices":
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			var req PatchMessagePolicyRequest
			require.NoError(t, dec.Decode(&req))
			require.NotNil(t, req.Mode)
			assert.Equal(t, "shadow", *req.Mode)
			assert.Equal(t, int64(1), req.ExpectedRevision)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(MessagePolicyResponse{
				MessagePolicy: store.MessagePolicy{
					ID:        "pol-1",
					ProjectID: "proj-1",
					Name:      "no-prose-choices",
					Enabled:   true,
					Mode:      "shadow",
					Scope:     store.MessagePolicyScope{Surfaces: []string{"agent_to_user"}},
					Revision:  2,
				},
				Coverage: cov,
			})

		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/projects/proj-1/message-policies/no-prose-choices":
			w.WriteHeader(http.StatusNoContent)

		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL, WithBearerToken("tok"))
	require.NoError(t, err)
	svc := c.ProjectMessagePolicies("proj-1")
	ctx := context.Background()

	created, err := svc.Create(ctx, &CreateMessagePolicyRequest{
		Name:              "no-prose-choices",
		Mode:              "enforce",
		Scope:             store.MessagePolicyScope{Surfaces: []string{"agent_to_user"}},
		ViolationRule:     "!message.has_choices && message.body.contains('Option A')",
		OnFailed:          "reject",
		RejectReason:      "Prose choices are not permitted.",
		RejectRemediation: "Send structured choice payloads.",
	})
	require.NoError(t, err)
	assert.Equal(t, "pol-1", created.ID)
	assert.Equal(t, cov.EnforcedSurfaces, created.Coverage.EnforcedSurfaces)
	assert.Equal(t, cov.UncoveredAgentPaths, created.Coverage.UncoveredAgentPaths)

	enabledTrue := true
	listed, err := svc.List(ctx, &ListMessagePoliciesOptions{
		Mode:    "enforce",
		Enabled: &enabledTrue,
		Limit:   25,
		Cursor:  "cur-1",
	})
	require.NoError(t, err)
	require.Len(t, listed.Policies, 1)
	assert.Equal(t, "cur-2", listed.NextCursor)
	assert.Equal(t, 1, listed.TotalCount)

	got, err := svc.Get(ctx, "no-prose-choices")
	require.NoError(t, err)
	assert.Equal(t, "pol-1", got.ID)

	shadowMode := "shadow"
	updated, err := svc.Update(ctx, "no-prose-choices", &PatchMessagePolicyRequest{
		Mode:             &shadowMode,
		ExpectedRevision: 1,
	})
	require.NoError(t, err)
	assert.Equal(t, "shadow", updated.Mode)
	assert.Equal(t, int64(2), updated.Revision)

	require.NoError(t, svc.Delete(ctx, "no-prose-choices"))
}

func TestMessagePolicies_TestAndDecisions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/message-policies/test":
			var req TestMessagePolicyRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			require.NotNil(t, req.Sample)
			assert.Equal(t, "Option A or Option B?", req.Sample.Body)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(TestMessagePolicyResponse{
				PolicyName:   "inline",
				Mode:         "enforce",
				Surface:      "agent_to_user",
				ScopeMatched: true,
				Class:        "violation",
				Outcome:      "reject",
				Action:       "rejected",
				Reason:       "Prose choices rejected.",
				Remediation:  "Revise message.",
				ElapsedMs:    1,
				BodyBytes:    len(req.Sample.Body),
			})

		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/proj-1/message-policies/pol-1/test":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(TestMessagePolicyResponse{
				PolicyID:       "pol-1",
				PolicyName:     "no-prose-choices",
				PolicyRevision: 2,
				Mode:           "shadow",
				Surface:        "agent_to_user",
				ScopeMatched:   true,
				Class:          "violation",
				Outcome:        "reject",
				Action:         "shadow",
				ElapsedMs:      2,
				BodyBytes:      21,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/message-policies/decisions":
			assert.Equal(t, "pol-1", r.URL.Query().Get("policy_id"))
			assert.Equal(t, "enforce", r.URL.Query().Get("mode"))
			assert.Equal(t, "agent_to_user", r.URL.Query().Get("surface"))
			assert.Equal(t, "violation", r.URL.Query().Get("class"))
			assert.Equal(t, "rejected", r.URL.Query().Get("action"))
			assert.Equal(t, "10", r.URL.Query().Get("limit"))
			assert.Equal(t, "c1", r.URL.Query().Get("cursor"))

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ListMessagePolicyDecisionsResponse{
				Decisions: []store.MessagePolicyDecision{
					{
						ID:             "dec-1",
						ProjectID:      "proj-1",
						PolicyID:       "pol-1",
						PolicyName:     "no-prose-choices",
						PolicyRevision: 1,
						Mode:           "enforce",
						Surface:        "agent_to_user",
						SenderKind:     "agent",
						SenderID:       "ag-1",
						RecipientKind:  "user",
						RecipientID:    "usr-1",
						Class:          "violation",
						Action:         "rejected",
						Reason:         "Prose choices rejected.",
					},
				},
				TotalCount: 1,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/proj-1/message-policies/pol-1/decisions":
			assert.Empty(t, r.URL.Query().Get("policy_id"))
			assert.Equal(t, "shadow", r.URL.Query().Get("action"))

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ListMessagePolicyDecisionsResponse{
				Decisions: []store.MessagePolicyDecision{
					{
						ID:        "dec-2",
						ProjectID: "proj-1",
						PolicyID:  "pol-1",
						Mode:      "shadow",
						MessageID: "msg-99",
						Class:     "violation",
						Action:    "shadow",
					},
				},
				TotalCount: 1,
			})

		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(t, err)
	svc := c.ProjectMessagePolicies("proj-1")
	ctx := context.Background()

	inlineRes, err := svc.Test(ctx, &TestMessagePolicyRequest{
		ViolationRule: "message.body.contains('Option A')",
		OnFailed:      "reject",
		Sample: &MessagePolicyTestSample{
			Surface: "agent_to_user",
			Body:    "Option A or Option B?",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "violation", inlineRes.Class)
	assert.Equal(t, "rejected", inlineRes.Action)

	storedRes, err := svc.TestPolicy(ctx, "pol-1", &TestMessagePolicyRequest{
		Sample: &MessagePolicyTestSample{
			Surface: "agent_to_user",
			Body:    "Option A or Option B?",
		},
	})
	require.NoError(t, err)
	assert.Equal(t, "pol-1", storedRes.PolicyID)
	assert.Equal(t, "shadow", storedRes.Action)

	decs, err := svc.ListDecisions(ctx, &ListMessagePolicyDecisionsOptions{
		PolicyID: "pol-1",
		Mode:     "enforce",
		Surface:  "agent_to_user",
		Class:    "violation",
		Action:   "rejected",
		Limit:    10,
		Cursor:   "c1",
	})
	require.NoError(t, err)
	require.Len(t, decs.Decisions, 1)
	assert.Equal(t, "dec-1", decs.Decisions[0].ID)

	polDecs, err := svc.ListPolicyDecisions(ctx, "pol-1", &ListMessagePolicyDecisionsOptions{
		PolicyID: "ignored-on-policy-route",
		Action:   "shadow",
	})
	require.NoError(t, err)
	require.Len(t, polDecs.Decisions, 1)
	assert.Equal(t, "msg-99", polDecs.Decisions[0].MessageID)
}

func TestMessagePolicies_WritesDoNotRetry(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"unavailable","message":"try later"}}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL, WithRetry(3, time.Millisecond))
	require.NoError(t, err)
	svc := c.ProjectMessagePolicies("proj-1")
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{"Create", func() error {
			_, err := svc.Create(ctx, &CreateMessagePolicyRequest{Name: "p", ViolationRule: "true", OnFailed: "admit"})
			return err
		}},
		{"Update", func() error {
			m := "shadow"
			_, err := svc.Update(ctx, "p", &PatchMessagePolicyRequest{Mode: &m})
			return err
		}},
		{"Delete", func() error {
			return svc.Delete(ctx, "p")
		}},
		{"Test", func() error {
			_, err := svc.Test(ctx, &TestMessagePolicyRequest{ViolationRule: "true", OnFailed: "admit"})
			return err
		}},
		{"TestPolicy", func() error {
			_, err := svc.TestPolicy(ctx, "p", &TestMessagePolicyRequest{Body: "hi"})
			return err
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits.Store(0)
			err := tc.call()
			require.Error(t, err)
			var apiErr *apiclient.APIError
			require.True(t, errors.As(err, &apiErr))
			assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
			assert.Equal(t, int32(1), hits.Load(), "%s must not retry on 5xx", tc.name)
		})
	}
}

func TestMessagePolicies_ErrorMappingAndLogRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_rule","message":"undeclared reference to 'foo'","details":{"field":"violation_rule","line":1,"column":8,"category":"unknown_variable"}}}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(t, err)

	_, err = c.ProjectMessagePolicies("proj-1").Create(context.Background(), &CreateMessagePolicyRequest{
		Name:          "bad",
		ViolationRule: "1 == 1 && foo",
		OnFailed:      "reject",
	})
	require.Error(t, err)
	var apiErr *apiclient.APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal(t, "invalid_rule", apiErr.Code)
	assert.Equal(t, "unknown_variable", apiErr.Details["category"])

	// Verify slog redacts sample body text.
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	secret := "SUPER-SECRET-SAMPLE-MESSAGE-BODY"
	logger.Info("testing sample",
		"req", TestMessagePolicyRequest{
			Name: "p",
			Sample: &MessagePolicyTestSample{
				Surface: "agent_to_user",
				Body:    secret,
			},
		},
		"sample", MessagePolicyTestSample{
			Surface: "agent_to_user",
			Body:    secret,
		},
	)
	assert.NotContains(t, buf.String(), secret)
}

func TestMessagePolicies_InputValidation(t *testing.T) {
	c, err := New("http://127.0.0.1:1")
	require.NoError(t, err)
	ctx := context.Background()

	_, err = c.ProjectMessagePolicies("").List(ctx, nil)
	require.Error(t, err)

	svc := c.ProjectMessagePolicies("proj-1")
	_, err = svc.Create(ctx, nil)
	require.Error(t, err)
	_, err = svc.Get(ctx, "")
	require.Error(t, err)
	_, err = svc.Update(ctx, "", &PatchMessagePolicyRequest{})
	require.Error(t, err)
	_, err = svc.Update(ctx, "p", nil)
	require.Error(t, err)
	require.Error(t, svc.Delete(ctx, ""))
	_, err = svc.Test(ctx, nil)
	require.Error(t, err)
	_, err = svc.TestPolicy(ctx, "", &TestMessagePolicyRequest{})
	require.Error(t, err)
	_, err = svc.TestPolicy(ctx, "p", nil)
	require.Error(t, err)
	_, err = svc.ListPolicyDecisions(ctx, "", nil)
	require.Error(t, err)
}
