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

package hub

// TestAgentKeysMessageBridge_ContractRows covers the remaining AK-numbered
// contract rows from the 4.1 test plan's A.4 that agent_keys_message_bridge_test.go
// does not already exercise: AK-38, AK-39, AK-44, AK-50, AK-51, AK-53, AK-54,
// AK-59 and AK-60. Each subtest names its row in its name.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/stretchr/testify/require"
)

// doRawBridgeRequest posts a hand-built JSON body (bytes, not a marshaled
// struct) to path as callerToken, through the full router. Several rows
// below need body shapes (type-mismatched fields, lone surrogates, trailing
// garbage) that cannot be produced by marshaling MessageRequest.
func doRawBridgeRequest(t *testing.T, srv *Server, path, callerToken string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callerToken)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// requireNoOperationID fails the test if the response's error envelope
// carries an operation_id key at all (contract §2.4a: omitted entirely, not
// an empty string, for a pre-operation-ID rejection).
func requireNoOperationID(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	env := decodeKeysError(t, rec.Body.Bytes())
	if _, ok := env.Details["operation_id"]; ok {
		t.Errorf("expected no operation_id key at all, got details: %v", env.Details)
	}
}

func TestAgentKeysMessageBridge_ContractRows(t *testing.T) {
	t.Run("AK-38_empty_msg_both_fields_400_no_op_id", func(t *testing.T) {
		for _, shape := range messageRouteShapes {
			t.Run(shape.name+"/top-level", func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("ak38-top-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), token, []byte(`{"raw":true,"message":""}`))
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				requireNoOperationID(t, rec)
				require.Equal(t, 0, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
			t.Run(shape.name+"/nested", func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("ak38-nested-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				body := []byte(`{"structured_message":{"raw":true,"msg":"","type":"instruction","version":1}}`)
				rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), token, body)
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				requireNoOperationID(t, rec)
				require.Equal(t, 0, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	})

	t.Run("AK-39_nul_byte_400_no_op_id", func(t *testing.T) {
		for _, shape := range messageRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("ak39-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), token, []byte(`{"raw":true,"message":"a\u0000b"}`))
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				requireNoOperationID(t, rec)
				require.Equal(t, 0, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	})

	t.Run("AK-44_ignored_fields_still_dispatch", func(t *testing.T) {
		for _, shape := range messageRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("ak44-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				body := []byte(`{"structured_message":{"raw":true,"msg":"C-c","version":1,"timestamp":"2026-01-01T00:00:00Z","type":"instruction","urgent":true,"status":"foo","sender":"user:someone","sender_id":"abc","recipient":"agent:` + f.agentInA.Slug + `"}}`)
				rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), token, body)
				require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
				var resp agentkeys.Response
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
				require.NotEmpty(t, resp.OperationID)
				require.Equal(t, 1, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	})

	t.Run("AK-50_large_plain_message_no_413", func(t *testing.T) {
		f, _, _, _ := newExecuteAgentKeysFixture(t)
		// A prior version used the literal
		// UTF-8 character (2 bytes each), producing a ~32 KB body, not the
		// ~96 KB the comment claimed -- and used a synthetic agentToken
		// caller, which has no store.Agent row and so fails
		// EvaluateAgentMessage's own sender lookup regardless of size,
		// landing on 403 message_denied. The test only asserted "!= 413",
		// so a request denied for an unrelated reason passed.
		//
		// Fixed: the literal 6-ASCII-byte JSON escape sequence (backslash,
		// u, 0, 0, e, 9 -- not the 2-byte UTF-8 character it decodes to)
		// repeated 16000 times gives 96,000 bytes of escapes alone,
		// matching the plan's "~96 KB"; and f.owner (a real human caller
		// with ownership-based self-access on f.agentInA) is used instead
		// of a synthetic agent token, so the request can actually reach
		// the ordinary message-delivery success path this row is about.
		escapedRunes := strings.Repeat("\\u00e9", 16000)
		body := []byte(`{"structured_message":{"msg":"` + escapedRunes + `","type":"instruction","version":1}}`)
		require.GreaterOrEqual(t, len(body), 96000, "body must be at least ~96 KB, matching the plan's size")

		userToken, _, _, err := f.srv.userTokenService.GenerateTokenPair(
			f.owner.ID, f.owner.Email, f.owner.DisplayName, f.owner.Role, ClientTypeWeb,
		)
		require.NoError(t, err)

		rec := doRawBridgeRequest(t, f.srv, "/api/v1/agents/"+f.agentInA.ID+"/message", userToken, body)
		require.Equal(t, http.StatusOK, rec.Code, "expected the ordinary message-delivery success path, body: %s", rec.Body.String())
		require.NotEqual(t, http.StatusRequestEntityTooLarge, rec.Code)
		// Unchanged (non-bridge) path: no migration headers.
		require.Empty(t, rec.Header().Get("X-Scion-Keys-Migration"))
	})

	t.Run("AK-51_cross_project_wake_precedence", func(t *testing.T) {
		for _, shape := range messageRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("ak51-"+shape.name), f.projectB.ID, ScopeAgentLifecycle)
				body := []byte(`{"raw":true,"wake":true,"message":"C-c"}`)
				rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), token, body)
				assertKeysDenialOutcome(t, shape.name, rec, http.StatusUnprocessableEntity, "cross_project_keys_unsupported")
				require.Equal(t, 0, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	})

	t.Run("AK-53_case_variant_spellings_bridge_dispatches", func(t *testing.T) {
		cases := []struct {
			name string
			body string
		}{
			{"top-level_RAW", `{"RAW":true,"message":"C-c"}`},
			{"nested_Raw", `{"structured_message":{"Raw":true,"msg":"C-c","type":"instruction","version":1}}`},
		}
		for _, shape := range messageRouteShapes {
			for _, tc := range cases {
				t.Run(shape.name+"/"+tc.name, func(t *testing.T) {
					f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
					token := f.agentToken(t, tid("ak53-"+shape.name+"-"+tc.name), f.projectA.ID, ScopeAgentLifecycle)
					rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), token, []byte(tc.body))
					require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
					require.Equal(t, "true", rec.Header().Get("Deprecation"), "must be handled by the bridge, not legacy delivery")
					require.Equal(t, 1, d.callCount())
					assertNoKeysSideEffects(t, storeSpy, events)
				})
			}
		}
	})

	t.Run("AK-54_trailing_bytes", func(t *testing.T) {
		t.Run("raw_object_bridge_dispatches", func(t *testing.T) {
			f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("ak54-raw"), f.projectA.ID, ScopeAgentLifecycle)
			body := []byte(`{"raw":true,"message":"C-c"} {"trailing":"garbage"}`)
			rec := doRawBridgeRequest(t, f.srv, "/api/v1/agents/"+f.agentInA.ID+"/message", token, body)
			require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
			require.Equal(t, "true", rec.Header().Get("Deprecation"))
			require.Equal(t, 1, d.callCount())
			assertNoKeysSideEffects(t, storeSpy, events)
		})
		t.Run("non_raw_object_unchanged_path", func(t *testing.T) {
			f, _, _, _ := newExecuteAgentKeysFixture(t)
			token := f.agentToken(t, tid("ak54-nonraw"), f.projectA.ID, ScopeAgentLifecycle)
			body := []byte(`{"structured_message":{"msg":"hello","type":"instruction","version":1}} {"trailing":"garbage"}`)
			rec := doRawBridgeRequest(t, f.srv, "/api/v1/agents/"+f.agentInA.ID+"/message", token, body)
			require.Empty(t, rec.Header().Get("X-Scion-Keys-Migration"), "must take the unchanged, non-bridge path")
			require.NotEqual(t, http.StatusRequestEntityTooLarge, rec.Code, "body: %s", rec.Body.String())
		})
	})

	t.Run("AK-59_type_mismatch_not_raw_post_auth_400", func(t *testing.T) {
		for _, shape := range messageRouteShapes {
			t.Run(shape.name, func(t *testing.T) {
				f, d, _, _ := newExecuteAgentKeysFixture(t)
				// The owner genuinely passes authorizeAgentMessage (self/
				// ownership exemption) via a real session token -- unlike a
				// synthetic agentToken caller, which has no store.Agent row
				// and so fails EvaluateAgentMessage's own sender lookup
				// before ever reaching the decode error this row targets.
				userToken, _, _, err := f.srv.userTokenService.GenerateTokenPair(
					f.owner.ID, f.owner.Email, f.owner.DisplayName, f.owner.Role, ClientTypeWeb,
				)
				require.NoError(t, err)
				body := []byte(`{"raw":true,"message":"hi","interrupt":"yes"}`)
				rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), userToken, body)

				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				require.Empty(t, rec.Header().Get("X-Scion-Keys-Migration"), "an undecodable body is not raw-selected; it must fall through")
				require.Equal(t, 0, d.callCount())
			})
		}
	})

	t.Run("AK-60_lone_surrogate_400_deterministic_either_order", func(t *testing.T) {
		// Using a plain "ok" for
		// structured_message.msg makes the two candidates disagree once
		// decoded, so the 400 comes from AK-41's message/structured-message
		// mismatch rule, not from lone-surrogate handling -- production was
		// never actually exercising the rule this row targets. The plan's
		// own body sets structured_message.msg to the literal U+FFFD
		// replacement character (not a "�" JSON escape): step 1's
		// lenient top-level decode (plain encoding/json, which silently
		// replaces message's lone \ud800 surrogate with U+FFFD) then makes
		// both candidates equal after that substitution, closing off the
		// AK-41 path entirely. The 400 must come from somewhere else: the
		// step 3-4 shadow-decode re-extracts message's *raw* JSON bytes
		// independently (not the already-substituted lenient value) and
		// agentkeys.ValidateKeysJSON rejects the lone surrogate in those raw
		// bytes directly -- the actual rule this row pins.
		literalReplacementChar := "�"
		for _, shape := range messageRouteShapes {
			t.Run(shape.name+"/message_first", func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("ak60-first-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				body := []byte(`{"raw":true,"message":"\ud800","structured_message":{"msg":"` + literalReplacementChar + `"}}`)
				rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), token, body)
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				require.Equal(t, 0, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
			t.Run(shape.name+"/message_last", func(t *testing.T) {
				f, d, storeSpy, events := newExecuteAgentKeysFixture(t)
				token := f.agentToken(t, tid("ak60-last-"+shape.name), f.projectA.ID, ScopeAgentLifecycle)
				body := []byte(`{"structured_message":{"msg":"` + literalReplacementChar + `"},"message":"\ud800","raw":true}`)
				rec := doRawBridgeRequest(t, f.srv, shape.path(f.agentInA), token, body)
				require.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
				require.Equal(t, 0, d.callCount())
				assertNoKeysSideEffects(t, storeSpy, events)
			})
		}
	})
}
