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

package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeFireAction struct {
	actionType  string
	permission  string
	detachesFn  func(req hubfire.FireRequest) bool
	validateFn  func(req hubfire.FireRequest) error
	authorizeFn func(ctx context.Context, req hubfire.FireRequest, auth FireAuthority, identity Identity) (FireAuthorized, *FireRefusal, error)
	fireFn      func(ctx context.Context, req hubfire.FireRequest, a FireAuthorized) (hubfire.FireOutcome, error)
}

func (a *fakeFireAction) Type() string       { return a.actionType }
func (a *fakeFireAction) Permission() string { return a.permission }

func (a *fakeFireAction) Validate(_ context.Context, _ Identity, _ string, payload string) (string, error) {
	return payload, nil
}

func (a *fakeFireAction) Detaches(req hubfire.FireRequest) bool {
	if a.detachesFn != nil {
		return a.detachesFn(req)
	}
	return false
}

func (a *fakeFireAction) ValidatePayload(req hubfire.FireRequest) error {
	if a.validateFn != nil {
		return a.validateFn(req)
	}
	return nil
}

func (a *fakeFireAction) Authorize(ctx context.Context, req hubfire.FireRequest, auth FireAuthority, identity Identity) (FireAuthorized, *FireRefusal, error) {
	if a.authorizeFn != nil {
		return a.authorizeFn(ctx, req, auth, identity)
	}
	return "authorized-token", nil, nil
}

func (a *fakeFireAction) Fire(ctx context.Context, req hubfire.FireRequest, authorized FireAuthorized) (hubfire.FireOutcome, error) {
	if a.fireFn != nil {
		return a.fireFn(ctx, req, authorized)
	}
	return hubfire.FireOutcome{Status: hubfire.FireStatusFired}, nil
}

func newFireCoreTestAuthority(ms *mockScheduledEventStore, projectID string) hubfire.RecordedAuthority {
	userID := seedEventHandlerMessageUser(ms, projectID)
	evt := withSessionRevision(store.ScheduledEvent{ID: "fire-1", ProjectID: projectID}, userID)
	return hubfire.RecordedAuthority{
		Attribution:    evt.InitiatorAttribution,
		Ceiling:        evt.AuthorityCeiling,
		DefinitionKind: hubfire.DefinitionKindTrigger,
		DefinitionID:   "trig-1",
		FireID:         "fire-1",
	}
}

func TestFireCoreOutcomeMapping(t *testing.T) {
	t.Parallel()

	const projectID = "project-firecore"

	t.Run("unknown action type returns failed", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  newFireCoreTestAuthority(ms, projectID),
			ActionType: "unknown_kind",
		})
		assert.Equal(t, hubfire.FireStatusFailed, out.Status)
		assert.Equal(t, "unknown action type: unknown_kind", out.Error)
	})

	t.Run("fired propagates ResultRef AfterRecord and ExecutorContext", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		rec := newFireCoreTestAuthority(ms, projectID)

		var seenExec hubfire.ExecutorContext
		var seenToken FireAuthorized
		afterCalled := false

		srv.RegisterAction(&fakeFireAction{
			actionType: "fake_send",
			permission: scheduledMessagePermission,
			authorizeFn: func(ctx context.Context, _ hubfire.FireRequest, auth FireAuthority, identity Identity) (FireAuthorized, *FireRefusal, error) {
				var ok bool
				seenExec, ok = ExecutorContextFromContext(ctx)
				require.True(t, ok)
				require.NotNil(t, identity)
				assert.Equal(t, rec.Attribution.InitiatorPrincipalID, auth.PrincipalID)
				assert.Equal(t, rec.DefinitionID, auth.Recorded.DefinitionID)
				return "resolved-target", nil, nil
			},
			fireFn: func(ctx context.Context, _ hubfire.FireRequest, a FireAuthorized) (hubfire.FireOutcome, error) {
				gotExec, ok := ExecutorContextFromContext(ctx)
				require.True(t, ok)
				assert.Equal(t, hubfire.ExecutorContext{Kind: "trigger_drainer", ID: "evt-9"}, gotExec)
				seenToken = a
				return hubfire.FireOutcome{
					Status:    hubfire.FireStatusFired,
					ResultRef: "msg-123",
					AfterRecord: func(context.Context) {
						afterCalled = true
					},
				}, nil
			},
		})

		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "fake_send",
			Payload:    `{"ok":true}`,
			Executor:   hubfire.ExecutorContext{Kind: "trigger_drainer", ID: "evt-9"},
		})
		assert.Equal(t, hubfire.FireStatusFired, out.Status)
		assert.Empty(t, out.Error)
		assert.Equal(t, "msg-123", out.ResultRef)
		assert.False(t, out.Detached)
		assert.Equal(t, hubfire.ExecutorContext{Kind: "trigger_drainer", ID: "evt-9"}, seenExec)
		assert.Equal(t, "resolved-target", seenToken)
		require.NotNil(t, out.AfterRecord)
		assert.False(t, afterCalled)
		out.AfterRecord(context.Background())
		assert.True(t, afterCalled)
	})

	t.Run("unrecorded authority returns refused with remedy text", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		srv.RegisterAction(&fakeFireAction{
			actionType: "message",
			permission: scheduledMessagePermission,
		})

		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindScheduledEvent, DefinitionID: "evt-1", FireID: "evt-1"},
			ActionType: "message",
			Payload:    `{"agentName":"worker","message":"hi"}`,
		})
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, errScheduledAuthorityUnrecorded.Error(), out.Error)
	})

	t.Run("denied authority returns generic refusal per action type", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		srv.RegisterAction(&fakeFireAction{
			actionType: "message",
			permission: scheduledMessagePermission,
		})
		srv.RegisterAction(&fakeFireAction{
			actionType: "custom_act",
			permission: scheduledMessagePermission,
		})

		missingPrincipal := withSessionRevision(store.ScheduledEvent{ID: "evt-missing", ProjectID: projectID}, "nonexistent-user")
		rec := hubfire.RecordedAuthority{
			Attribution:    missingPrincipal.InitiatorAttribution,
			Ceiling:        missingPrincipal.AuthorityCeiling,
			DefinitionKind: hubfire.DefinitionKindTrigger,
			DefinitionID:   "trig-2",
			FireID:         "fire-2",
		}

		msgOut := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "message",
		})
		assert.Equal(t, hubfire.FireStatusRefused, msgOut.Status)
		assert.Equal(t, errScheduledMessageRefused.Error(), msgOut.Error)

		customOut := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "custom_act",
		})
		assert.Equal(t, hubfire.FireStatusRefused, customOut.Status)
		assert.Equal(t, "custom_act refused", customOut.Error)
	})

	t.Run("Authorize refusal vs operational error", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		rec := newFireCoreTestAuthority(ms, projectID)

		fireCalled := false
		srv.RegisterAction(&fakeFireAction{
			actionType: "refuse_act",
			permission: scheduledMessagePermission,
			authorizeFn: func(context.Context, hubfire.FireRequest, FireAuthority, Identity) (FireAuthorized, *FireRefusal, error) {
				return nil, &FireRefusal{Reason: "target refused"}, nil
			},
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				fireCalled = true
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}, nil
			},
		})
		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "refuse_act",
		})
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, "target refused", out.Error)
		assert.False(t, fireCalled)

		srv.RegisterAction(&fakeFireAction{
			actionType: "auth_err_act",
			permission: scheduledMessagePermission,
			authorizeFn: func(context.Context, hubfire.FireRequest, FireAuthority, Identity) (FireAuthorized, *FireRefusal, error) {
				return nil, nil, errors.New("db unavailable")
			},
		})
		errOut := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "auth_err_act",
		})
		assert.Equal(t, hubfire.FireStatusFailed, errOut.Status)
		assert.Equal(t, "db unavailable", errOut.Error)
	})

	t.Run("Fire skipped outcome skip sentinel failed error and Detached", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		rec := newFireCoreTestAuthority(ms, projectID)

		srv.RegisterAction(&fakeFireAction{
			actionType: "skip_outcome",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				return hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: "target not running"}, nil
			},
		})
		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "skip_outcome",
		})
		assert.Equal(t, hubfire.FireStatusSkipped, out.Status)
		assert.Equal(t, "target not running", out.Error)

		srv.RegisterAction(&fakeFireAction{
			actionType: "skip_sentinel",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				return hubfire.FireOutcome{}, skipScheduledFire("capacity full")
			},
		})
		out = srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "skip_sentinel",
		})
		assert.Equal(t, hubfire.FireStatusSkipped, out.Status)
		assert.Equal(t, "capacity full", out.Error)

		srv.RegisterAction(&fakeFireAction{
			actionType: "fire_err",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				return hubfire.FireOutcome{}, errors.New("broker timeout")
			},
		})
		out = srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "fire_err",
		})
		assert.Equal(t, hubfire.FireStatusFailed, out.Status)
		assert.Equal(t, "broker timeout", out.Error)

		srv.RegisterAction(&fakeFireAction{
			actionType: "detached_act",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired, Detached: true}, nil
			},
		})
		out = srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: "detached_act",
		})
		assert.Equal(t, hubfire.FireStatusFired, out.Status)
		assert.True(t, out.Detached)
	})
}

func TestFireCorePayloadValidatedBeforeAuthority(t *testing.T) {
	t.Parallel()

	t.Run("fake FirePayloadValidator short-circuits before authority", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)

		authorizeCalled := false
		srv.RegisterAction(&fakeFireAction{
			actionType: "message",
			permission: scheduledMessagePermission,
			validateFn: func(req hubfire.FireRequest) error {
				if req.Payload != `{"valid":true}` {
					return errors.New("invalid message payload: bad json")
				}
				return nil
			},
			authorizeFn: func(context.Context, hubfire.FireRequest, FireAuthority, Identity) (FireAuthorized, *FireRefusal, error) {
				authorizeCalled = true
				return nil, nil, nil
			},
		})

		// Unrecorded authority + invalid payload: payload validation runs first and
		// returns failed rather than errScheduledAuthorityUnrecorded.
		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  "project-order",
			Authority:  hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindScheduledEvent, FireID: "evt-bad"},
			ActionType: "message",
			Payload:    `{not-json`,
		})
		assert.Equal(t, hubfire.FireStatusFailed, out.Status)
		assert.Equal(t, "invalid message payload: bad json", out.Error)
		assert.False(t, authorizeCalled)
	})

	t.Run("real messageAction validates payload before unrecorded authority refusal", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		srv.registerFireActions()

		unrecorded := hubfire.RecordedAuthority{
			DefinitionKind: hubfire.DefinitionKindScheduledEvent,
			DefinitionID:   "evt-unrec",
			FireID:         "evt-unrec",
		}

		cases := []struct {
			name       string
			projectID  string
			payload    string
			wantStatus hubfire.FireStatus
			wantErrSub string
		}{
			{
				name:       "malformed JSON fails before authority",
				projectID:  "project-order",
				payload:    `{not-json`,
				wantStatus: hubfire.FireStatusFailed,
				wantErrSub: "invalid message payload:",
			},
			{
				name:       "empty message fails before authority",
				projectID:  "project-order",
				payload:    `{"agentName":"worker","message":""}`,
				wantStatus: hubfire.FireStatusFailed,
				wantErrSub: "message payload is empty",
			},
			{
				name:       "missing target fails before authority",
				projectID:  "project-order",
				payload:    `{"message":"hello"}`,
				wantStatus: hubfire.FireStatusFailed,
				wantErrSub: "message payload must include agentId or agentName",
			},
			{
				name:       "agentName without projectID fails before authority",
				projectID:  "",
				payload:    `{"agentName":"worker","message":"hello"}`,
				wantStatus: hubfire.FireStatusFailed,
				wantErrSub: "message payload must include agentId or agentName",
			},
			{
				name:       "valid payload reaches authority resolution and refuses on unrecorded authority",
				projectID:  "project-order",
				payload:    `{"agentName":"worker","message":"hello"}`,
				wantStatus: hubfire.FireStatusRefused,
				wantErrSub: errScheduledAuthorityUnrecorded.Error(),
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out := srv.fireAction(context.Background(), hubfire.FireRequest{
					ProjectID:  tc.projectID,
					Authority:  unrecorded,
					ActionType: "message",
					Payload:    tc.payload,
				})
				assert.Equal(t, tc.wantStatus, out.Status)
				assert.Contains(t, out.Error, tc.wantErrSub)
				if tc.wantStatus == hubfire.FireStatusFailed {
					assert.NotContains(t, out.Error, errScheduledAuthorityUnrecorded.Error())
				}
			})
		}
	})

	t.Run("real execAction validates payload before unrecorded authority refusal", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		setFireCoreExperiments(t, srv, map[string]bool{
			conduitExperiment:         true,
			experiments.ExecSchedules: true,
		})
		srv.registerFireActions()

		unrecorded := hubfire.RecordedAuthority{
			DefinitionKind: hubfire.DefinitionKindScheduledEvent,
			DefinitionID:   "evt-exec-unrec",
			FireID:         "evt-exec-unrec",
		}

		cases := []struct {
			name       string
			payload    string
			wantStatus hubfire.FireStatus
			wantErr    string
		}{
			{
				name:       "malformed JSON fails before authority",
				payload:    `{not-json`,
				wantStatus: hubfire.FireStatusFailed,
				wantErr:    errScheduledExecPayloadInvalid.Error(),
			},
			{
				name:       "empty command fails before authority",
				payload:    `{"agentId":"a1","command":[]}`,
				wantStatus: hubfire.FireStatusFailed,
				wantErr:    errScheduledExecPayloadInvalid.Error(),
			},
			{
				name:       "missing agentId fails before authority",
				payload:    `{"agentName":"worker","command":["true"]}`,
				wantStatus: hubfire.FireStatusFailed,
				wantErr:    errScheduledExecPayloadInvalid.Error(),
			},
			{
				name:       "wake true fails before authority",
				payload:    `{"agentId":"a1","command":["true"],"wake":true}`,
				wantStatus: hubfire.FireStatusFailed,
				wantErr:    errScheduledExecPayloadInvalid.Error(),
			},
			{
				name:       "unknown field fails before authority",
				payload:    `{"agentId":"a1","command":["true"],"extra":1}`,
				wantStatus: hubfire.FireStatusFailed,
				wantErr:    errScheduledExecPayloadInvalid.Error(),
			},
			{
				name:       "valid payload reaches authority resolution and refuses with exec refused",
				payload:    `{"agentId":"a1","command":["true"]}`,
				wantStatus: hubfire.FireStatusRefused,
				wantErr:    execReasonRefused,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				out := srv.fireAction(context.Background(), hubfire.FireRequest{
					ProjectID:  "project-order",
					Authority:  unrecorded,
					ActionType: scheduledEventTypeExec,
					Payload:    tc.payload,
				})
				assert.Equal(t, tc.wantStatus, out.Status)
				assert.Equal(t, tc.wantErr, out.Error)
			})
		}
	})
}

func TestFireCoreTracker(t *testing.T) {
	// Not t.Parallel: subtest captures process-default slog logger.
	t.Run("Detach survives caller cancel and observes fire abort", func(t *testing.T) {
		srv := newEventHandlerTestServer(newMockStore())
		tracker := newScheduledFireTracker(srv, "evt-detach")

		abortCtx, cancelAbort := context.WithCancel(context.Background())
		defer cancelAbort()
		callerCtx, cancelCaller := context.WithCancel(withScheduledFireAbort(context.Background(), abortCtx))

		detached, cancelDetached := tracker.Detach(callerCtx)
		defer cancelDetached()

		cancelCaller()
		assert.NoError(t, detached.Err(), "detached context must outlive caller cancellation")

		cancelAbort()
		require.Eventually(t, func() bool {
			return detached.Err() != nil
		}, time.Second, 5*time.Millisecond)
	})

	t.Run("Admit registers with scheduledDetachedFires and refuses after stop", func(t *testing.T) {
		srv := newEventHandlerTestServer(newMockStore())
		tracker := newScheduledFireTracker(srv, "evt-admit")

		admittedCtx, done, ok := tracker.Admit(context.Background())
		require.True(t, ok)
		assert.Len(t, srv.runningDetachedFires.snapshot(), 1)
		done()
		assert.Empty(t, srv.runningDetachedFires.snapshot())
		assert.Error(t, admittedCtx.Err())

		srv.scheduledDetachedFires.stop(context.Background(), 0)
		_, _, ok = tracker.Admit(context.Background())
		assert.False(t, ok)
	})
}

func TestFireCoreContainment(t *testing.T) {
	t.Parallel()

	coreFiles := map[string][]string{
		"firecore_authority.go": {
			"resolveRecordedAuthority",
			"fireRevisionCeilingConsistent",
			"resolveFireUser",
			"resolveFireAgent",
			"fireStanding",
			"fireCreatorName",
			"fireEffectCeiling",
		},
		"firecore_tracker.go": {
			"newScheduledFireTracker",
		},
		"firecore_action.go": {
			"RegisterAction",
			"LookupAction",
			"fireAction",
		},
		"action_message.go": {
			"Validate",
			"ValidatePayload",
			"Authorize",
			"Fire",
			"Detaches",
			"deliverMessageAction",
		},
		"action_exec.go": {
			"Validate",
			"ValidatePayload",
			"Preauthorize",
			"OnAuthorityRefused",
			"Authorize",
			"Fire",
			"Detaches",
			"authorizeExecFire",
			"dispatchExecAction",
		},
		"action_dispatch_agent.go": {
			"Validate",
			"ValidatePayload",
			"Authorize",
			"Fire",
			"Detaches",
			"authorizeDispatchAgentFire",
		},
		"agent_create_headless.go": {
			"createAgentHeadless",
		},
	}
	forbiddenStoreTypes := map[string]bool{
		"ScheduledEvent": true,
		"Schedule":       true,
		"Trigger":        true,
	}

	fset := token.NewFileSet()
	for file, funcs := range coreFiles {
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		require.NoError(t, err, file)
		found := map[string]bool{}
		for _, decl := range parsed.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				found[fn.Name.Name] = true
			}
		}
		for _, want := range funcs {
			assert.True(t, found[want], "%s not found in %s", want, file)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "sourceEffectCeiling" {
				t.Errorf("%s calls sourceEffectCeiling at %s", file, fset.Position(sel.Pos()))
			}
			if pkgIdent, ok := sel.X.(*ast.Ident); ok && pkgIdent.Name == "store" && forbiddenStoreTypes[sel.Sel.Name] {
				t.Errorf("%s references store.%s at %s", file, sel.Sel.Name, fset.Position(sel.Pos()))
			}
			return true
		})
	}
}

func TestScheduledFireError(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		eventType    string
		out          hubfire.FireOutcome
		wantNil      bool
		wantRecorded bool
		wantSkip     bool
		wantText     string
	}{
		{
			name:         "Detached fired",
			eventType:    "exec",
			out:          hubfire.FireOutcome{Status: hubfire.FireStatusFired, Detached: true},
			wantRecorded: true,
		},
		{
			name:         "Detached skipped",
			eventType:    "exec",
			out:          hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: "capacity", Detached: true},
			wantRecorded: true,
			wantSkip:     true,
			wantText:     "capacity",
		},
		{
			name:         "Detached failed",
			eventType:    "exec",
			out:          hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "dispatch failed", Detached: true},
			wantRecorded: true,
			wantText:     "dispatch failed",
		},
		{
			name:         "Detached refused",
			eventType:    "exec",
			out:          hubfire.FireOutcome{Status: hubfire.FireStatusRefused, Error: "exec refused", Detached: true},
			wantRecorded: true,
			wantText:     "exec refused",
		},
		{
			name:      "not-Detached fired",
			eventType: "message",
			out:       hubfire.FireOutcome{Status: hubfire.FireStatusFired},
			wantNil:   true,
		},
		{
			name:      "not-Detached skipped",
			eventType: "exec",
			out:       hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: "not running"},
			wantSkip:  true,
			wantText:  "not running",
		},
		{
			name:      "not-Detached failed",
			eventType: "message",
			out:       hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: "boom"},
			wantText:  "boom",
		},
		{
			name:      "not-Detached refused",
			eventType: "message",
			out:       hubfire.FireOutcome{Status: hubfire.FireStatusRefused, Error: errScheduledMessageRefused.Error()},
			wantText:  errScheduledMessageRefused.Error(),
		},
		{
			name:      "not-Detached unknown action type maps to unknown event type",
			eventType: "mystery",
			out:       hubfire.FireOutcome{Status: hubfire.FireStatusFailed, Error: unknownActionTypeError("mystery")},
			wantText:  "unknown event type: mystery",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := scheduledFireError(tc.eventType, tc.out)
			if tc.wantNil {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			recErr, recorded := asOutcomeRecorded(err)
			assert.Equal(t, tc.wantRecorded, recorded)
			inner := err
			if recorded {
				if tc.wantText == "" && !tc.wantSkip {
					assert.NoError(t, recErr)
					return
				}
				require.Error(t, recErr)
				inner = recErr
			}
			reason, skipped := asScheduledSkip(inner)
			assert.Equal(t, tc.wantSkip, skipped)
			if tc.wantSkip {
				assert.Equal(t, tc.wantText, reason)
			} else {
				assert.Equal(t, tc.wantText, inner.Error())
			}
		})
	}
}

type fireCoreLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *fireCoreLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *fireCoreLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type heldAgentMockStore struct {
	*mockScheduledEventStore
	heldID string
}

func (s *heldAgentMockStore) HasActiveAgentHold(_ context.Context, agentID string) (bool, error) {
	return agentID == s.heldID, nil
}

func TestFireCoreRefusalLogging(t *testing.T) {
	// Not t.Parallel: captures process-default slog logger.
	const projectID = "project-refusal-log"

	logBuf := &fireCoreLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	ms := newMockStore()
	hs := &heldAgentMockStore{mockScheduledEventStore: ms, heldID: "agent-held"}
	srv := newEventHandlerTestServer(hs)
	srv.RegisterAction(&fakeFireAction{
		actionType: "custom_hook",
		permission: "agent.create",
	})

	// 1. logFireAuthorityRefusal logs neutral "deferred fire authority denied"
	// with actionType and permission fields.
	out := srv.fireAction(context.Background(), hubfire.FireRequest{
		ProjectID:  projectID,
		Authority:  hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindTrigger, DefinitionID: "trig-9", FireID: "fire-9"},
		ActionType: "custom_hook",
	})
	assert.Equal(t, hubfire.FireStatusRefused, out.Status)
	logs := logBuf.String()
	assert.Contains(t, logs, "deferred fire authority denied")
	assert.Contains(t, logs, "actionType=custom_hook")
	assert.Contains(t, logs, "permission=agent.create")
	assert.Contains(t, logs, "projectID="+projectID)

	// 1b. Scheduled message authority denial preserves the scheduler log message.
	logBuf.mu.Lock()
	logBuf.buf.Reset()
	logBuf.mu.Unlock()
	srv.RegisterAction(&messageAction{srv: srv})
	schedMsgOut := srv.fireAction(context.Background(), hubfire.FireRequest{
		ProjectID:  projectID,
		Authority:  hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindScheduledEvent, DefinitionID: "evt-9", FireID: "evt-9"},
		ActionType: "message",
		Payload:    `{"agentName":"worker","message":"hi"}`,
	})
	assert.Equal(t, hubfire.FireStatusRefused, schedMsgOut.Status)
	assert.Contains(t, logBuf.String(), "scheduled message authority denied at fire time")

	// 2. fireStanding preserves projectID in refusal logs on both the core
	// FireAuthority path and the fireStandingForTest helper path.
	rec := newFireCoreTestAuthority(ms, projectID)
	auth, _, err := srv.resolveRecordedAuthority(context.Background(), rec, projectID, scheduledMessagePermission)
	require.NoError(t, err)
	logBuf.mu.Lock()
	logBuf.buf.Reset()
	logBuf.mu.Unlock()

	err = srv.fireStanding(context.Background(), auth, &store.Agent{ID: "agent-held", ProjectID: projectID})
	require.ErrorIs(t, err, errScheduledMessageRefused)
	assert.Contains(t, logBuf.String(), "projectID="+projectID)

	logBuf.mu.Lock()
	logBuf.buf.Reset()
	logBuf.mu.Unlock()
	err = fireStandingForTest(context.Background(), srv, store.ScheduledEvent{
		ID:        "evt-shim-1",
		ProjectID: "project-shim-log",
		EventType: "message",
	}, &store.Agent{ID: "agent-held", ProjectID: "project-shim-log"})
	require.ErrorIs(t, err, errScheduledMessageRefused)
	assert.Contains(t, logBuf.String(), "projectID=project-shim-log")
}

func TestFireCoreAuthorityMissingRuleDenialText(t *testing.T) {
	t.Parallel()

	const projectID = "project-missing-rule"
	ms := newMockStore()
	srv := newEventHandlerTestServer(ms)
	rec := newFireCoreTestAuthority(ms, projectID)

	_, _, err := srv.resolveRecordedAuthority(context.Background(), rec, projectID, "")
	require.ErrorIs(t, err, errScheduledAuthorityDenied)
	require.ErrorIs(t, err, errNoRecordedAuthorityRule)
	assert.ErrorContains(t, err, "action has no recorded authority rule")

	evt := store.ScheduledEvent{
		ID:                   "evt-unmapped",
		ProjectID:            projectID,
		EventType:            "unmapped_type",
		InitiatorAttribution: rec.Attribution,
		AuthorityCeiling:     rec.Ceiling,
	}
	_, _, err = resolveScheduledAuthorityForTest(context.Background(), srv, evt)
	require.ErrorIs(t, err, errScheduledAuthorityDenied)
	assert.ErrorContains(t, err, "action has no recorded authority rule")

	// Ceiling consistency still runs before the permission-rule check.
	evt.AuthorityCeiling = store.EffectCeiling{}
	_, _, err = resolveScheduledAuthorityForTest(context.Background(), srv, evt)
	require.ErrorIs(t, err, errScheduledAuthorityUnrecorded)
}

type fireCoreScheduleStore struct {
	*mockScheduledEventStore
	advanceCalls        int
	afterRunCalls       int
	completeCalls       int
	unadmittedExecCalls int
	lastRunErr          string
}

func (s *fireCoreScheduleStore) AdvanceScheduleNextRun(_ context.Context, id string, nextRunAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.advanceCalls++
	if s.schedules == nil {
		s.schedules = make(map[string]*store.Schedule)
	}
	if sc, ok := s.schedules[id]; ok {
		cp := nextRunAt
		sc.NextRunAt = &cp
	}
	return nil
}

func (s *fireCoreScheduleStore) UpdateScheduleAfterRun(_ context.Context, id string, ranAt time.Time, nextRunAt time.Time, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.afterRunCalls++
	s.lastRunErr = errMsg
	if sc, ok := s.schedules[id]; ok {
		cpRan := ranAt
		cpNext := nextRunAt
		sc.LastRunAt = &cpRan
		sc.NextRunAt = &cpNext
		sc.RunCount++
		if errMsg != "" {
			sc.ErrorCount++
			sc.LastRunStatus = store.ScheduleRunError
			sc.LastRunError = errMsg
		} else {
			sc.LastRunStatus = store.ScheduleRunSuccess
			sc.LastRunError = ""
		}
	}
	return nil
}

func (s *fireCoreScheduleStore) CompleteFiringScheduledEvent(ctx context.Context, id string, status string, firedAt time.Time, errMsg string, scheduleID string, ranAt time.Time) (bool, error) {
	s.mu.Lock()
	s.completeCalls++
	s.mu.Unlock()
	return s.mockScheduledEventStore.CompleteFiringScheduledEvent(ctx, id, status, firedAt, errMsg, scheduleID, ranAt)
}

func (s *fireCoreScheduleStore) CompleteUnadmittedExecEvent(_ context.Context, id string, status string, errMsg string, scheduleID string, ranAt time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unadmittedExecCalls++
	evt, ok := s.events[id]
	if !ok || (evt.Status != store.ScheduledEventFired && evt.Status != store.ScheduledEventExpired) {
		return false, nil
	}
	evt.Status = status
	evt.Error = errMsg
	if scheduleID != "" {
		if sc, ok := s.schedules[scheduleID]; ok {
			sc.RunCount++
			if status == store.ScheduledEventFailed {
				sc.ErrorCount++
			}
			if sc.LastRunAt == nil || sc.LastRunAt.Before(ranAt) {
				cpRan := ranAt
				sc.LastRunAt = &cpRan
				switch status {
				case store.ScheduledEventSkipped:
					sc.LastRunStatus = store.ScheduleRunSkipped
					sc.LastRunError = errMsg
				case store.ScheduledEventFailed:
					sc.LastRunStatus = store.ScheduleRunError
					sc.LastRunError = errMsg
				default:
					sc.LastRunStatus = store.ScheduleRunSuccess
					sc.LastRunError = ""
				}
			}
		}
	}
	return true, nil
}

func TestFireCoreSchedulerAdapters(t *testing.T) {
	t.Parallel()

	const projectID = "project-sched-adapter"

	t.Run("one-shot fireEvent dispatches Server.actions outcomes AfterRecord and Detached", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		sched := NewScheduler(ms, slog.Default())
		sched.srv = srv
		srv.scheduler = sched

		rec := newFireCoreTestAuthority(ms, projectID)
		baseEvt := store.ScheduledEvent{
			ProjectID:            projectID,
			FireAt:               time.Now().UTC(),
			Payload:              `{"ok":true}`,
			Status:               store.ScheduledEventPending,
			InitiatorAttribution: rec.Attribution,
			AuthorityCeiling:     rec.Ceiling,
		}

		statusAtAfter := ""
		srv.RegisterAction(&fakeFireAction{
			actionType: "act_fired",
			permission: scheduledMessagePermission,
			fireFn: func(ctx context.Context, req hubfire.FireRequest, _ FireAuthorized) (hubfire.FireOutcome, error) {
				gotExec, ok := ExecutorContextFromContext(ctx)
				require.True(t, ok)
				assert.Equal(t, ExecutorContext{Kind: "scheduler", ID: "scheduled_event:" + req.Authority.FireID}, gotExec)
				require.NotNil(t, req.Tracked)
				return hubfire.FireOutcome{
					Status: hubfire.FireStatusFired,
					AfterRecord: func(context.Context) {
						statusAtAfter = ms.getEvent(req.Authority.FireID).Status
					},
				}, nil
			},
		})
		srv.RegisterAction(&fakeFireAction{
			actionType: "act_skipped",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				return hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: "window missed"}, nil
			},
		})
		srv.RegisterAction(&fakeFireAction{
			actionType: "act_detached",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired, Detached: true}, nil
			},
		})
		srv.RegisterAction(&fakeFireAction{
			actionType: "act_detached_skip",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				return hubfire.FireOutcome{Status: hubfire.FireStatusSkipped, Error: "cap", Detached: true}, nil
			},
		})

		assert.ElementsMatch(t, []string{"act_fired", "act_skipped", "act_detached", "act_detached_skip"}, sched.Status().EventHandlers)

		firedEvt := baseEvt
		firedEvt.ID = "evt-fired"
		firedEvt.EventType = "act_fired"
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &firedEvt))
		sched.fireEvent(context.Background(), firedEvt, false)
		assert.Equal(t, store.ScheduledEventFired, ms.getEvent("evt-fired").Status)
		assert.Equal(t, store.ScheduledEventFired, statusAtAfter, "AfterRecord runs after fireEvent persists status")

		skipEvt := baseEvt
		skipEvt.ID = "evt-skipped"
		skipEvt.EventType = "act_skipped"
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &skipEvt))
		sched.fireEvent(context.Background(), skipEvt, false)
		gotSkip := ms.getEvent("evt-skipped")
		assert.Equal(t, store.ScheduledEventSkipped, gotSkip.Status)
		assert.Equal(t, "window missed", gotSkip.Error)

		detEvt := baseEvt
		detEvt.ID = "evt-detached"
		detEvt.EventType = "act_detached"
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &detEvt))
		sched.fireEvent(context.Background(), detEvt, false)
		assert.Equal(t, store.ScheduledEventPending, ms.getEvent("evt-detached").Status, "Detached leaves recording to tracker")

		detSkipEvt := baseEvt
		detSkipEvt.ID = "evt-detached-skip"
		detSkipEvt.EventType = "act_detached_skip"
		detSkipEvt.Status = store.ScheduledEventFailed
		detSkipEvt.Error = "sweeper recorded failure"
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &detSkipEvt))
		sched.fireEvent(context.Background(), detSkipEvt, false)
		gotDetSkip := ms.getEvent("evt-detached-skip")
		assert.Equal(t, store.ScheduledEventFailed, gotDetSkip.Status, "Detached skip must not overwrite handler/sweeper outcome")
		assert.Equal(t, "sweeper recorded failure", gotDetSkip.Error)

		unkEvt := baseEvt
		unkEvt.ID = "evt-unknown"
		unkEvt.EventType = "no_such_type"
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &unkEvt))
		sched.fireEvent(context.Background(), unkEvt, false)
		gotUnk := ms.getEvent("evt-unknown")
		assert.Equal(t, store.ScheduledEventFailed, gotUnk.Status)
		assert.Equal(t, "unknown event type: no_such_type", gotUnk.Error)
	})

	t.Run("single registry: Server.RegisterAction overrides default legacy registration for scheduler and evaluator", func(t *testing.T) {
		ms := newMockStore()
		fs := &fireCoreScheduleStore{mockScheduledEventStore: ms}
		srv := newEventHandlerTestServer(fs)
		srv.registerFireActions()
		sched := NewScheduler(fs, slog.Default())
		sched.srv = srv
		srv.scheduler = sched
		srv.registerSchedulerHandlers()

		// Override "message" on Server ONLY; no scheduler registration can shadow it.
		var serverActionCalls atomic.Int32
		srv.RegisterAction(&fakeFireAction{
			actionType: "message",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				serverActionCalls.Add(1)
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired}, nil
			},
		})

		rec := newFireCoreTestAuthority(ms, projectID)
		now := time.Now().UTC()
		evt1 := store.ScheduledEvent{
			ID:                   "single-reg-oneshot",
			ProjectID:            projectID,
			EventType:            "message",
			FireAt:               now,
			Payload:              `{"agentName":"worker","message":"hi"}`,
			Status:               store.ScheduledEventPending,
			InitiatorAttribution: rec.Attribution,
			AuthorityCeiling:     rec.Ceiling,
		}
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &evt1))
		sched.fireEvent(context.Background(), evt1, false)
		assert.Equal(t, int32(1), serverActionCalls.Load(), "one-shot scheduler fired Server-registered action")
		assert.Equal(t, store.ScheduledEventFired, ms.getEvent("single-reg-oneshot").Status)

		evt2 := evt1
		evt2.ID = "single-reg-recurring"
		evt2.ScheduleID = "sched-single-reg"
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &evt2))
		srv.runScheduleFire(context.Background(), slog.Default(),
			store.Schedule{ID: "sched-single-reg", ProjectID: projectID, EventType: "message"},
			evt2, now, now.Add(time.Hour), nil)
		assert.Equal(t, int32(2), serverActionCalls.Load(), "recurring evaluator fired Server-registered action")
		assert.Equal(t, store.ScheduledEventFired, ms.getEvent("single-reg-recurring").Status)
	})

	t.Run("recurring runScheduleFire dispatches FireAction outcomes AfterRecord and Detached", func(t *testing.T) {
		ms := newMockStore()
		fs := &fireCoreScheduleStore{mockScheduledEventStore: ms}
		srv := newEventHandlerTestServer(fs)
		srv.scheduler = NewScheduler(fs, slog.Default())
		srv.scheduler.srv = srv

		rec := newFireCoreTestAuthority(ms, projectID)
		now := time.Now().UTC()
		next := now.Add(time.Hour)

		statusAtAfter := ""
		srv.RegisterAction(&fakeFireAction{
			actionType: "rec_fired",
			permission: scheduledMessagePermission,
			fireFn: func(ctx context.Context, req hubfire.FireRequest, _ FireAuthorized) (hubfire.FireOutcome, error) {
				gotExec, ok := ExecutorContextFromContext(ctx)
				require.True(t, ok)
				assert.Equal(t, ExecutorContext{Kind: "schedule_evaluator", ID: "schedule:sched-1"}, gotExec)
				assert.Equal(t, hubfire.DefinitionKindSchedule, req.Authority.DefinitionKind)
				assert.Equal(t, "sched-1", req.Authority.DefinitionID)
				return hubfire.FireOutcome{
					Status: hubfire.FireStatusFired,
					AfterRecord: func(context.Context) {
						statusAtAfter = ms.getEvent(req.Authority.FireID).Status
					},
				}, nil
			},
		})
		srv.RegisterAction(&fakeFireAction{
			actionType: "rec_detached",
			permission: scheduledMessagePermission,
			fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
				return hubfire.FireOutcome{Status: hubfire.FireStatusFired, Detached: true}, nil
			},
		})

		schedRow := store.Schedule{ID: "sched-1", ProjectID: projectID, EventType: "rec_fired"}
		evt := store.ScheduledEvent{
			ID:                   "rec-evt-1",
			ProjectID:            projectID,
			ScheduleID:           "sched-1",
			EventType:            "rec_fired",
			FireAt:               now,
			Status:               store.ScheduledEventPending,
			InitiatorAttribution: rec.Attribution,
			AuthorityCeiling:     rec.Ceiling,
		}
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &evt))
		srv.runScheduleFire(context.Background(), slog.Default(), schedRow, evt, now, next, nil)
		assert.Equal(t, store.ScheduledEventFired, ms.getEvent("rec-evt-1").Status)
		assert.Equal(t, store.ScheduledEventFired, statusAtAfter)
		assert.Equal(t, 1, fs.afterRunCalls)
		assert.Empty(t, fs.lastRunErr)

		detSched := store.Schedule{ID: "sched-2", ProjectID: projectID, EventType: "rec_detached"}
		detEvt := evt
		detEvt.ID = "rec-evt-2"
		detEvt.ScheduleID = "sched-2"
		detEvt.EventType = "rec_detached"
		detEvt.Status = store.ScheduledEventFiring
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &detEvt))
		srv.runScheduleFire(context.Background(), slog.Default(), detSched, detEvt, now, next,
			&detachedFireRun{eventID: "rec-evt-2", scheduleID: "sched-2", ranAt: now})
		assert.Equal(t, store.ScheduledEventFiring, ms.getEvent("rec-evt-2").Status, "pre-detached + Detached leaves recording to tracker")
		assert.Equal(t, 1, fs.afterRunCalls, "UpdateScheduleAfterRun skipped when pre-detached and Detached are true")

		unkSched := store.Schedule{ID: "sched-3", ProjectID: projectID, EventType: "missing_rec"}
		unkEvt := evt
		unkEvt.ID = "rec-evt-3"
		unkEvt.ScheduleID = "sched-3"
		unkEvt.EventType = "missing_rec"
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &unkEvt))
		srv.runScheduleFire(context.Background(), slog.Default(), unkSched, unkEvt, now, next, nil)
		assert.Equal(t, store.ScheduledEventFailed, ms.getEvent("rec-evt-3").Status)
		assert.Equal(t, "unknown event type: missing_rec", ms.getEvent("rec-evt-3").Error)
		assert.Equal(t, 2, fs.afterRunCalls)
		assert.Equal(t, "unknown event type: missing_rec", fs.lastRunErr)
	})
}

func TestFireCoreActionDetaches(t *testing.T) {
	t.Parallel()

	realMsgAction := &messageAction{}
	realExecAction := &execAction{}
	realDispAction := &dispatchAgentAction{}

	for _, defKind := range []string{
		hubfire.DefinitionKindScheduledEvent,
		hubfire.DefinitionKindSchedule,
	} {
		assert.True(t, realMsgAction.Detaches(hubfire.FireRequest{
			Authority: hubfire.RecordedAuthority{DefinitionKind: defKind},
			Payload:   `{"agentName":"worker","message":"hi","wake":true}`,
		}), "messageAction wake=true detaches (%s)", defKind)
		assert.False(t, realMsgAction.Detaches(hubfire.FireRequest{
			Authority: hubfire.RecordedAuthority{DefinitionKind: defKind},
			Payload:   `{"agentName":"worker","message":"hi","wake":false}`,
		}), "messageAction wake=false does not detach (%s)", defKind)
		assert.False(t, realMsgAction.Detaches(hubfire.FireRequest{
			Authority: hubfire.RecordedAuthority{DefinitionKind: defKind},
			Payload:   `{"agentName":"worker","message":"hi"}`,
		}), "messageAction wake omitted does not detach (%s)", defKind)
		assert.False(t, realMsgAction.Detaches(hubfire.FireRequest{
			Authority: hubfire.RecordedAuthority{DefinitionKind: defKind},
			Payload:   `{bad-json`,
		}), "messageAction malformed payload does not detach (%s)", defKind)

		assert.False(t, realDispAction.Detaches(hubfire.FireRequest{
			Authority: hubfire.RecordedAuthority{DefinitionKind: defKind},
			Payload:   `{"agentName":"worker"}`,
		}), "dispatchAgentAction never detaches (%s)", defKind)
	}

	assert.True(t, realExecAction.Detaches(hubfire.FireRequest{
		Authority: hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindSchedule},
		Payload:   `{"agentId":"a1","command":["true"]}`,
	}), "execAction recurring exec detaches when DefinitionKind == schedule (rev 8.12)")
	assert.False(t, realExecAction.Detaches(hubfire.FireRequest{
		Authority: hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindScheduledEvent},
		Payload:   `{"agentId":"a1","command":["true"]}`,
	}), "execAction one-shot exec keeps non-detaching path")
	assert.False(t, realExecAction.Detaches(hubfire.FireRequest{
		Authority: hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindTrigger},
		Payload:   `{"agentId":"a1","command":["true"]}`,
	}), "execAction trigger exec does not use schedule pre-fire detachment")
}

func TestFireCoreExecActionValidate(t *testing.T) {
	t.Parallel()

	act := &execAction{srv: newEventHandlerTestServer(newMockStore())}
	assert.Equal(t, scheduledEventTypeExec, act.Type())
	assert.Equal(t, scheduledExecPermission, act.Permission())

	normalized, err := act.Validate(context.Background(), nil, "proj-1", `{"agentId":"a1","command":["true"]}`)
	require.ErrorContains(t, err, "exec action authoring validation is not wired in Stage 1")
	assert.Empty(t, normalized)

	var nilAction *execAction
	normalized, err = nilAction.Validate(context.Background(), nil, "", "")
	require.ErrorContains(t, err, "exec action authoring validation is not wired in Stage 1")
	assert.Empty(t, normalized)
}

func TestFireCoreMessageActionValidate(t *testing.T) {
	t.Parallel()

	const projectID = "project-msg-validate"
	ms := newMockStore()
	srv := newEventHandlerTestServer(ms)
	userID := seedEventHandlerMessageUser(ms, projectID)
	user, err := ms.GetUser(context.Background(), userID)
	require.NoError(t, err)
	caller := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, store.UserRoleAdmin, "session")

	require.NoError(t, ms.CreateAgent(context.Background(), &store.Agent{
		ID:        "agent-msg-val-1",
		Slug:      "worker-one",
		Name:      "worker-one",
		ProjectID: projectID,
		OwnerID:   user.ID,
		Phase:     "running",
	}))

	act := &messageAction{srv: srv}
	normalized, err := act.Validate(context.Background(), caller, projectID, `{"agentName":"worker-one","message":"hello","wake":true}`)
	require.NoError(t, err)
	var decoded MessageEventPayload
	require.NoError(t, json.Unmarshal([]byte(normalized), &decoded))
	assert.Equal(t, "agent-msg-val-1", decoded.AgentID)
	assert.Equal(t, "worker-one", decoded.AgentName)
	assert.Equal(t, "hello", decoded.Message)
	assert.True(t, decoded.Wake)

	// Unresolved target is refused in strict trigger mode (404).
	normalized, err = act.Validate(context.Background(), caller, projectID, `{"agentName":"no-such-agent","message":"hello"}`)
	require.Error(t, err)
	var authErr *messageAuthoringError
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, 404, authErr.httpStatus)
	assert.Equal(t, "Agent", authErr.notFoundResource)
	assert.Empty(t, normalized)

	var nilAction *messageAction
	normalized, err = nilAction.Validate(context.Background(), nil, "", "")
	require.ErrorContains(t, err, "message action: server not configured")
	assert.Empty(t, normalized)
}

type dispatchDenyEdgeStore struct {
	*mockScheduledEventStore
	edge *store.DelegationEdge
}

func (s *dispatchDenyEdgeStore) GetDelegationEdgesForDelegate(_ context.Context, _, id string) ([]*store.DelegationEdge, error) {
	if s.edge != nil && s.edge.DelegateID == id {
		return []*store.DelegationEdge{s.edge}, nil
	}
	return nil, nil
}

func TestFireCoreDispatchAgentAction(t *testing.T) {
	t.Parallel()

	const projectID = "project-dispatch-action"

	seedDispatchUser := func(ms *mockScheduledEventStore, perms ...string) string {
		userID := "dispatch-user-1"
		ms.projects[projectID] = &store.Project{ID: projectID, Name: projectID, Slug: projectID}
		ms.users[userID] = &store.User{
			ID:          userID,
			Email:       "dispatch-user@example.com",
			DisplayName: "Dispatch User",
			Role:        store.UserRoleMember,
			Status:      store.UserStatusActive,
		}
		ms.roleDefinitions["rd-dispatch"] = &store.RoleDefinition{
			ID:          "rd-dispatch",
			Name:        store.ProjectRoleMember,
			ScopeType:   store.RoleScopeProject,
			Permissions: perms,
			System:      true,
		}
		ms.roleBindings = append(ms.roleBindings, &store.RoleBinding{
			ID:               "rb-dispatch",
			RoleDefinitionID: "rd-dispatch",
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      userID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          projectID,
		})
		return userID
	}

	t.Run("Validate refuses in Stage 1 and Detaches is false", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		act := &dispatchAgentAction{srv: srv}

		assert.Equal(t, "dispatch_agent", act.Type())
		assert.Equal(t, "agent.create", act.Permission())
		assert.False(t, act.Detaches(hubfire.FireRequest{
			Authority: hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindSchedule},
		}))
		assert.False(t, act.Detaches(hubfire.FireRequest{
			Authority: hubfire.RecordedAuthority{DefinitionKind: hubfire.DefinitionKindTrigger},
		}))

		normalized, err := act.Validate(context.Background(), nil, projectID, `{"agentName":"worker"}`)
		require.ErrorContains(t, err, "dispatch_agent action authoring validation is not wired in Stage 1")
		assert.Empty(t, normalized)

		var nilAction *dispatchAgentAction
		normalized, err = nilAction.Validate(context.Background(), nil, "", "")
		require.ErrorContains(t, err, "dispatch_agent action authoring validation is not wired in Stage 1")
		assert.Empty(t, normalized)
	})

	t.Run("ValidatePayload rejects invalid JSON missing agentName and non-name template", func(t *testing.T) {
		act := &dispatchAgentAction{srv: newEventHandlerTestServer(newMockStore())}
		require.ErrorContains(t, act.ValidatePayload(hubfire.FireRequest{Payload: `{bad`}), "invalid dispatch_agent payload")
		require.ErrorContains(t, act.ValidatePayload(hubfire.FireRequest{Payload: `{}`}), "agentName is required")
		require.ErrorContains(t, act.ValidatePayload(hubfire.FireRequest{
			Payload: `{"agentName":"worker","template":"https://evil.example/t.git"}`,
		}), templateRefRefusal)
		require.NoError(t, act.ValidatePayload(hubfire.FireRequest{
			Payload: `{"agentName":"worker","template":"claude","noAuth":true}`,
		}))
	})

	t.Run("CanDelegate refusal differentiates trigger vs schedule refusal text and audit targetType", func(t *testing.T) {
		// Grant agent.create so resolveRecordedAuthority and agentCreateDecision pass,
		// but omit project.secret_read so CanDelegate(AgentRoleFull) is denied.
		for _, tc := range []struct {
			name           string
			defKind        string
			wantTargetType string
			wantExactErr   string
			wantErrSub     string
		}{
			{
				name:           "trigger_dispatch",
				defKind:        hubfire.DefinitionKindTrigger,
				wantTargetType: "trigger_dispatch",
				wantExactErr:   "dispatch_agent refused",
			},
			{
				name:           "scheduled_dispatch",
				defKind:        hubfire.DefinitionKindScheduledEvent,
				wantTargetType: "scheduled_dispatch",
				wantErrSub:     "failed CanDelegate at fire time",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ms := newMockStore()
				ms.projects[projectID] = &store.Project{ID: projectID, Name: projectID, Slug: projectID}
				creatorID := seedFullRoleDispatchCreator(ms, projectID)
				ms.roleDefinitions["rd-creator-member"].Permissions = []string{"agent.read", "agent.create"}
				st := &dispatchDenyEdgeStore{
					mockScheduledEventStore: ms,
					edge: &store.DelegationEdge{
						ID:            "edge-creator",
						DelegatorType: store.DelegationPrincipalUser,
						DelegatorID:   "creator-owner",
						DelegateType:  store.DelegationPrincipalAgent,
						DelegateID:    creatorID,
						ScopeType:     store.RoleScopeProject,
						ScopeID:       projectID,
						Role:          string(AgentRoleFull),
						Active:        true,
						AuthorityProvenance: store.AuthorityProvenance{
							ProvenanceVersion:    store.ProvenanceVersionV1,
							SourcePrincipalKind:  store.DelegationPrincipalUser,
							SourcePrincipalID:    "creator-owner",
							SourceCredentialKind: store.SourceCredentialUAT,
							SourceCredentialID:   "uat-narrow",
						},
						EffectCeiling: store.EffectCeiling{
							Kind:              store.EffectCeilingBounded,
							Version:           permissions.CeilingVersionV1,
							PermissionIDs:     []string{"agent.create"},
							BoundaryKind:      string(permissions.BoundaryKindProject),
							BoundaryProjectID: projectID,
						},
					},
				}
				srv := newEventHandlerTestServer(st)
				srv.registerFireActions()
				evt := withMockAgentRevision(store.ScheduledEvent{ID: "fire-deny", ProjectID: projectID, CreatedBy: creatorID}, creatorID)

				out := srv.fireAction(context.Background(), hubfire.FireRequest{
					ProjectID: projectID,
					Authority: hubfire.RecordedAuthority{
						Attribution:    evt.InitiatorAttribution,
						Ceiling:        evt.AuthorityCeiling,
						DefinitionKind: tc.defKind,
						DefinitionID:   "def-1",
						FireID:         "fire-deny",
					},
					ActionType: "dispatch_agent",
					Payload:    `{"agentName":"worker-denied"}`,
				})
				assert.Equal(t, hubfire.FireStatusRefused, out.Status)
				if tc.wantExactErr != "" {
					assert.Equal(t, tc.wantExactErr, out.Error)
				} else {
					assert.Contains(t, out.Error, tc.wantErrSub)
				}
				srv.mutationAuditWork.stop(context.Background(), time.Second, time.Second)
				ms.mu.Lock()
				audits := append([]*store.MutationAuditRecord(nil), ms.audits...)
				ms.mu.Unlock()
				require.Len(t, audits, 1)
				assert.Equal(t, tc.wantTargetType, audits[0].TargetType)
				assert.Equal(t, "fire-deny", audits[0].TargetID)
				assert.Equal(t, "deny", audits[0].CanDelegateResult)
			})
		}
	})

	t.Run("Fire creates detached owned agent and returns ResultRef", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		srv.registerFireActions()
		userID := seedDispatchUser(ms,
			"agent.create",
			"project.read",
			"project.secret_read",
			"agent.message",
			"scheduled_event.create",
			"scheduled_event.read",
			"scheduled_event.list",
		)
		// Cap project role to baseline so standard member permissions satisfy CanDelegate.
		ms.projects[projectID].Annotations = map[string]string{
			projectSettingDefaultAgentRole: string(AgentRoleBaseline),
		}
		evt := withSessionRevision(store.ScheduledEvent{ID: "fire-ok", ProjectID: projectID}, userID)

		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID: projectID,
			Authority: hubfire.RecordedAuthority{
				Attribution:    evt.InitiatorAttribution,
				Ceiling:        evt.AuthorityCeiling,
				DefinitionKind: hubfire.DefinitionKindScheduledEvent,
				DefinitionID:   "evt-ok",
				FireID:         "fire-ok",
			},
			ActionType: "dispatch_agent",
			Payload:    `{"agentName":"worker-ok","task":"run checks"}`,
		})
		require.Equal(t, hubfire.FireStatusFired, out.Status, "unexpected fire error: %s", out.Error)
		require.NotEmpty(t, out.ResultRef)

		created, err := ms.GetAgent(context.Background(), out.ResultRef)
		require.NoError(t, err)
		assert.Equal(t, "worker-ok", created.Slug)
		assert.Equal(t, userID, created.OwnerID)
		assert.Equal(t, []string{userID}, created.Ancestry)
		assert.True(t, created.Detached)
		require.NotNil(t, created.AppliedConfig)
		assert.Equal(t, string(AgentRoleBaseline), created.AppliedConfig.AgentRole)
		assert.False(t, created.AppliedConfig.NoAuth)
	})

	t.Run("missing project refuses with same error text for schedule and trigger", func(t *testing.T) {
		wantErr := `project "project-dispatch-action" no longer exists`
		for _, defKind := range []string{
			hubfire.DefinitionKindScheduledEvent,
			hubfire.DefinitionKindSchedule,
			hubfire.DefinitionKindTrigger,
		} {
			t.Run(defKind, func(t *testing.T) {
				ms := newMockStore()
				srv := newEventHandlerTestServer(ms)
				srv.registerFireActions()
				userID := seedDispatchUser(ms,
					"agent.create",
					"project.read",
					"project.secret_read",
					"agent.message",
					"scheduled_event.create",
					"scheduled_event.read",
					"scheduled_event.list",
				)
				creatorID := seedFullRoleDispatchCreator(ms, projectID)
				delete(ms.projects, projectID)

				userEvt := withSessionRevision(store.ScheduledEvent{ID: "fire-noproj-user", ProjectID: projectID}, userID)
				userOut := srv.fireAction(context.Background(), hubfire.FireRequest{
					ProjectID: projectID,
					Authority: hubfire.RecordedAuthority{
						Attribution:    userEvt.InitiatorAttribution,
						Ceiling:        userEvt.AuthorityCeiling,
						DefinitionKind: defKind,
						DefinitionID:   "def-noproj-user",
						FireID:         "fire-noproj-user",
					},
					ActionType: "dispatch_agent",
					Payload:    `{"agentName":"worker-noproj-user"}`,
				})
				assert.Equal(t, hubfire.FireStatusRefused, userOut.Status)
				assert.Equal(t, wantErr, userOut.Error)

				agentEvt := withMockAgentRevision(store.ScheduledEvent{ID: "fire-noproj-agent", ProjectID: projectID, CreatedBy: creatorID}, creatorID)
				agentOut := srv.fireAction(context.Background(), hubfire.FireRequest{
					ProjectID: projectID,
					Authority: hubfire.RecordedAuthority{
						Attribution:    agentEvt.InitiatorAttribution,
						Ceiling:        agentEvt.AuthorityCeiling,
						DefinitionKind: defKind,
						DefinitionID:   "def-noproj-agent",
						FireID:         "fire-noproj-agent",
					},
					ActionType: "dispatch_agent",
					Payload:    `{"agentName":"worker-noproj-agent"}`,
				})
				assert.Equal(t, hubfire.FireStatusRefused, agentOut.Status)
				assert.Equal(t, wantErr, agentOut.Error)

				srv.mutationAuditWork.stop(context.Background(), time.Second, time.Second)
				ms.mu.Lock()
				audits := append([]*store.MutationAuditRecord(nil), ms.audits...)
				ms.mu.Unlock()
				assert.Empty(t, audits)
				_, err := ms.GetAgentBySlug(context.Background(), projectID, "worker-noproj-user")
				assert.ErrorIs(t, err, store.ErrNotFound)
				_, err = ms.GetAgentBySlug(context.Background(), projectID, "worker-noproj-agent")
				assert.ErrorIs(t, err, store.ErrNotFound)
			})
		}
	})
}

// TestFireCoreAdapterDetachesMatrix covers all seven cells of design §5.1a
// (rev 8.11 / 8.12): recurring x {detaches, !detaches} x {Detached, !Detached}
// (including the mismatch cell !detaches && Detached), one-shot x
// {Detached, detaches && !Detached, !detaches && !Detached}, and recurring +
// one-shot exec detachment on execAction.
func TestFireCoreAdapterDetachesMatrix(t *testing.T) {
	// Not t.Parallel: captures process-default slog logger for mismatch check.
	const projectID = "project-detaches-matrix"
	now := time.Date(2026, 11, 16, 2, 0, 0, 0, time.UTC)
	wantNextRun := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)

	cells := []struct {
		name           string
		recurring      bool
		detaches       bool
		outDetached    bool
		wantEvtStatus  string
		wantEvtErrSub  string
		wantAdvance    int
		wantAfterRun   int
		wantComplete   int
		wantRunCount   int
		wantNextRunSet bool
		wantLogSub     string
	}{
		{
			name:           "1. recurring detaches=true Detached=true leaves write to tracker after pre-advancing next_run_at",
			recurring:      true,
			detaches:       true,
			outDetached:    true,
			wantEvtStatus:  store.ScheduledEventFiring,
			wantAdvance:    1,
			wantAfterRun:   0,
			wantComplete:   0,
			wantRunCount:   0,
			wantNextRunSet: true,
		},
		{
			name:           "2. recurring detaches=true Detached=false records via once-write without touching next_run_at",
			recurring:      true,
			detaches:       true,
			outDetached:    false,
			wantEvtStatus:  store.ScheduledEventFired,
			wantAdvance:    1,
			wantAfterRun:   0,
			wantComplete:   1,
			wantRunCount:   1,
			wantNextRunSet: true,
		},
		{
			name:           "3. recurring detaches=false Detached=false uses synchronous UpdateScheduledEventStatus and UpdateScheduleAfterRun",
			recurring:      true,
			detaches:       false,
			outDetached:    false,
			wantEvtStatus:  store.ScheduledEventFired,
			wantAdvance:    0,
			wantAfterRun:   1,
			wantComplete:   0,
			wantRunCount:   1,
			wantNextRunSet: true,
		},
		{
			name:           "4. recurring detaches=false Detached=true mismatch logs records failed and advances next_run_at",
			recurring:      true,
			detaches:       false,
			outDetached:    true,
			wantEvtStatus:  store.ScheduledEventFailed,
			wantEvtErrSub:  "detached fire outcome without pre-fire Detaches declaration",
			wantAdvance:    0,
			wantAfterRun:   1,
			wantComplete:   0,
			wantRunCount:   1,
			wantNextRunSet: true,
			wantLogSub:     "detached fire outcome without pre-fire Detaches declaration",
		},
		{
			name:          "5. one-shot Detached=true maps to outcomeRecorded without overwriting event row",
			recurring:     false,
			detaches:      false,
			outDetached:   true,
			wantEvtStatus: store.ScheduledEventPending,
			wantComplete:  0,
		},
		{
			name:          "6. one-shot detaches=true Detached=false claims firing and records via once-write",
			recurring:     false,
			detaches:      true,
			outDetached:   false,
			wantEvtStatus: store.ScheduledEventFired,
			wantComplete:  1,
		},
		{
			name:          "7. one-shot detaches=false Detached=false updates event status synchronously",
			recurring:     false,
			detaches:      false,
			outDetached:   false,
			wantEvtStatus: store.ScheduledEventFired,
			wantComplete:  0,
		},
	}

	for _, tc := range cells {
		t.Run(tc.name, func(t *testing.T) {
			logBuf := &fireCoreLogBuffer{}
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })

			ms := newMockStore()
			ms.schedules = make(map[string]*store.Schedule)
			fs := &fireCoreScheduleStore{mockScheduledEventStore: ms}
			srv := newEventHandlerTestServer(fs)
			sched := NewScheduler(fs, slog.Default())
			sched.srv = srv
			srv.scheduler = sched

			rec := newFireCoreTestAuthority(ms, projectID)
			afterCalled := false
			srv.RegisterAction(&fakeFireAction{
				actionType: "matrix_act",
				permission: scheduledMessagePermission,
				detachesFn: func(hubfire.FireRequest) bool { return tc.detaches },
				fireFn: func(ctx context.Context, req hubfire.FireRequest, _ FireAuthorized) (hubfire.FireOutcome, error) {
					if !tc.recurring && tc.detaches {
						_, done, ok := req.Tracked.Admit(ctx)
						require.True(t, ok)
						defer done()
						assert.Equal(t, store.ScheduledEventFiring, ms.getEvent(req.Authority.FireID).Status)
					}
					if tc.recurring && tc.detaches {
						fs.mu.Lock()
						adv := fs.advanceCalls
						fs.mu.Unlock()
						assert.Equal(t, 1, adv, "AdvanceScheduleNextRun ran before Fire")
					}
					return hubfire.FireOutcome{
						Status:      hubfire.FireStatusFired,
						Detached:    tc.outDetached,
						AfterRecord: func(context.Context) { afterCalled = true },
					}, nil
				},
			})

			if tc.recurring {
				dueAt := now.Add(-time.Minute)
				scRow := &store.Schedule{
					ID:                   "sched-cell",
					ProjectID:            projectID,
					Name:                 "matrix-sched",
					CronExpr:             "0 * * * *",
					EventType:            "matrix_act",
					Payload:              `{"ok":true}`,
					Status:               store.ScheduleStatusActive,
					NextRunAt:            &dueAt,
					InitiatorAttribution: rec.Attribution,
					AuthorityCeiling:     rec.Ceiling,
				}
				ms.schedules[scRow.ID] = scRow

				srv.executeSchedule(context.Background(), *scRow, now)
				srv.scheduledDetachedFires.wg.Wait()

				if !tc.detaches && tc.outDetached {
					assert.False(t, afterCalled, "F2: mismatch cell must not run AfterRecord for a fire recorded failed")
				} else {
					assert.True(t, afterCalled, "AfterRecord should run for non-mismatch recurring cells")
				}

				fs.mu.Lock()
				assert.Equal(t, tc.wantAdvance, fs.advanceCalls, "AdvanceScheduleNextRun calls")
				assert.Equal(t, tc.wantAfterRun, fs.afterRunCalls, "UpdateScheduleAfterRun calls")
				assert.Equal(t, tc.wantComplete, fs.completeCalls, "CompleteFiringScheduledEvent calls")
				require.Len(t, ms.events, 1)
				var gotEvt *store.ScheduledEvent
				for _, ev := range ms.events {
					gotEvt = ev
				}
				gotSched := *ms.schedules[scRow.ID]
				fs.mu.Unlock()

				assert.Equal(t, tc.wantEvtStatus, gotEvt.Status)
				if tc.wantEvtErrSub != "" {
					assert.Contains(t, gotEvt.Error, tc.wantEvtErrSub)
					assert.Equal(t, store.ScheduleRunError, gotSched.LastRunStatus)
					assert.Contains(t, gotSched.LastRunError, tc.wantEvtErrSub)
				}
				assert.Equal(t, tc.wantRunCount, gotSched.RunCount)
				if tc.wantNextRunSet {
					require.NotNil(t, gotSched.NextRunAt)
					assert.True(t, gotSched.NextRunAt.Equal(wantNextRun), "next_run_at=%v want=%v", gotSched.NextRunAt, wantNextRun)
				}
				if tc.wantLogSub != "" {
					assert.Contains(t, logBuf.String(), tc.wantLogSub)
				}
				return
			}

			evt := store.ScheduledEvent{
				ID:                   "evt-cell",
				ProjectID:            projectID,
				EventType:            "matrix_act",
				FireAt:               now,
				Payload:              `{"ok":true}`,
				Status:               store.ScheduledEventPending,
				InitiatorAttribution: rec.Attribution,
				AuthorityCeiling:     rec.Ceiling,
			}
			require.NoError(t, ms.CreateScheduledEvent(context.Background(), &evt))
			sched.fireEvent(context.Background(), evt, false)

			fs.mu.Lock()
			gotComplete := fs.completeCalls
			fs.mu.Unlock()
			assert.Equal(t, tc.wantComplete, gotComplete, "CompleteFiringScheduledEvent calls")
			assert.Equal(t, tc.wantEvtStatus, ms.getEvent("evt-cell").Status)
		})
	}

	t.Run("8a. recurring exec admitted detaches pre-advances next_run_at and leaves write to exec tracker", func(t *testing.T) {
		ms := newMockStore()
		ms.schedules = make(map[string]*store.Schedule)
		fs := &fireCoreScheduleStore{mockScheduledEventStore: ms}
		srv := newEventHandlerTestServer(fs)
		sched := NewScheduler(fs, slog.Default())
		sched.srv = srv
		srv.scheduler = sched

		require.NoError(t, ms.CreateAgent(context.Background(), &store.Agent{
			ID:        "a1",
			Slug:      "worker",
			ProjectID: projectID,
			Phase:     "running",
		}))
		rec := newFireCoreTestAuthority(ms, projectID)
		srv.RegisterAction(newTestFireAction(scheduledEventTypeExec, func(_ context.Context, req hubfire.FireRequest) error {
			fs.mu.Lock()
			adv := fs.advanceCalls
			fs.mu.Unlock()
			assert.Equal(t, 1, adv, "AdvanceScheduleNextRun ran before recurring exec handler")
			gotEvt := ms.getEvent(req.Authority.FireID)
			require.NotNil(t, gotEvt)
			assert.Equal(t, store.ScheduledEventFired, gotEvt.Status)
			return outcomeRecorded(nil)
		}))

		dueAt := now.Add(-time.Minute)
		scRow := &store.Schedule{
			ID:                   "sched-rec-exec-admitted",
			ProjectID:            projectID,
			Name:                 "rec-exec-admitted",
			CronExpr:             "0 * * * *",
			EventType:            scheduledEventTypeExec,
			Payload:              `{"agentId":"a1","command":["true"]}`,
			Status:               store.ScheduleStatusActive,
			NextRunAt:            &dueAt,
			InitiatorAttribution: rec.Attribution,
			AuthorityCeiling:     rec.Ceiling,
		}
		ms.schedules[scRow.ID] = scRow

		srv.executeSchedule(context.Background(), *scRow, now)
		srv.scheduledDetachedFires.wg.Wait()

		// Simulate shutdown sweep: run.tryRecord(func(){}) already claimed the
		// detachedFireRun, so recordAbandonedDetachedFires must not clobber the event.
		srv.recordAbandonedDetachedFires(context.Background())

		fs.mu.Lock()
		assert.Equal(t, 1, fs.advanceCalls, "AdvanceScheduleNextRun called once before fire")
		assert.Equal(t, 0, fs.afterRunCalls, "UpdateScheduleAfterRun must not run for detached exec")
		assert.Equal(t, 0, fs.completeCalls, "CompleteFiringScheduledEvent must not run for exec")
		assert.Equal(t, 0, fs.unadmittedExecCalls, "CompleteUnadmittedExecEvent must not run for admitted exec")
		require.Len(t, ms.events, 1)
		var gotEvt *store.ScheduledEvent
		for _, ev := range ms.events {
			gotEvt = ev
		}
		gotSched := *ms.schedules[scRow.ID]
		fs.mu.Unlock()

		assert.Equal(t, store.ScheduledEventFired, gotEvt.Status)
		assert.True(t, gotEvt.FireAt.Equal(dueAt), "recurring exec preserves NextRunAt on evt.FireAt")
		assert.Equal(t, 0, gotSched.RunCount, "admitted exec run owns schedule run accounting")
		require.NotNil(t, gotSched.NextRunAt)
		assert.True(t, gotSched.NextRunAt.Equal(wantNextRun))
	})

	t.Run("8b. recurring exec unadmitted skip and refusal record via CompleteUnadmittedExecEvent without rewinding next_run_at", func(t *testing.T) {
		ms := newMockStore()
		ms.schedules = make(map[string]*store.Schedule)
		fs := &fireCoreScheduleStore{mockScheduledEventStore: ms}
		srv := newEventHandlerTestServer(fs)
		srv.registerFireActions()
		sched := NewScheduler(fs, slog.Default())
		sched.srv = srv
		srv.scheduler = sched

		require.NoError(t, ms.CreateAgent(context.Background(), &store.Agent{
			ID:        "a1",
			Slug:      "worker",
			ProjectID: projectID,
			Phase:     "running",
		}))
		rec := newFireCoreTestAuthority(ms, projectID)
		dueAt := now.Add(-time.Minute)
		scRow := &store.Schedule{
			ID:                   "sched-rec-exec-skip",
			ProjectID:            projectID,
			Name:                 "rec-exec-skip",
			CronExpr:             "0 * * * *",
			EventType:            scheduledEventTypeExec,
			Payload:              `{"agentId":"a1","command":["true"]}`,
			Status:               store.ScheduleStatusActive,
			NextRunAt:            &dueAt,
			InitiatorAttribution: rec.Attribution,
			AuthorityCeiling:     rec.Ceiling,
		}
		ms.schedules[scRow.ID] = scRow

		// With hub.exec_schedules disabled by default on newEventHandlerTestServer,
		// execAction.Preauthorize returns unadmitted skip ("exec schedules disabled").
		srv.executeSchedule(context.Background(), *scRow, now)
		srv.scheduledDetachedFires.wg.Wait()

		fs.mu.Lock()
		assert.Equal(t, 1, fs.advanceCalls, "AdvanceScheduleNextRun called once before fire")
		assert.Equal(t, 0, fs.afterRunCalls, "UpdateScheduleAfterRun must not run for detached exec")
		assert.Equal(t, 0, fs.completeCalls, "CompleteFiringScheduledEvent must not run for exec")
		assert.Equal(t, 1, fs.unadmittedExecCalls, "CompleteUnadmittedExecEvent records unadmitted recurring exec")
		require.Len(t, ms.events, 1)
		var gotEvt *store.ScheduledEvent
		for _, ev := range ms.events {
			gotEvt = ev
		}
		gotSched := *ms.schedules[scRow.ID]
		fs.mu.Unlock()

		assert.Equal(t, store.ScheduledEventSkipped, gotEvt.Status)
		assert.Equal(t, execReasonDisabled, gotEvt.Error)
		assert.Equal(t, 1, gotSched.RunCount)
		assert.Equal(t, 0, gotSched.ErrorCount, "skipped recurring exec must not increment error_count")
		assert.Equal(t, store.ScheduleRunSkipped, gotSched.LastRunStatus)
		assert.Equal(t, execReasonDisabled, gotSched.LastRunError)
		require.NotNil(t, gotSched.NextRunAt)
		assert.True(t, gotSched.NextRunAt.Equal(wantNextRun))
	})

	t.Run("8c. one-shot exec stays non-detaching for both admitted and unadmitted outcomes", func(t *testing.T) {
		ms := newMockStore()
		fs := &fireCoreScheduleStore{mockScheduledEventStore: ms}
		srv := newEventHandlerTestServer(fs)
		sched := NewScheduler(fs, slog.Default())
		sched.srv = srv
		srv.scheduler = sched

		rec := newFireCoreTestAuthority(ms, projectID)
		admitted := true
		srv.RegisterAction(newTestFireAction(scheduledEventTypeExec, func(_ context.Context, _ hubfire.FireRequest) error {
			if admitted {
				return outcomeRecorded(nil)
			}
			return skipScheduledFire("not running")
		}))

		evt1 := store.ScheduledEvent{
			ID:                   "evt-oneshot-exec-admitted",
			ProjectID:            projectID,
			EventType:            scheduledEventTypeExec,
			FireAt:               now,
			Payload:              `{"agentId":"a1","command":["true"]}`,
			Status:               store.ScheduledEventPending,
			InitiatorAttribution: rec.Attribution,
			AuthorityCeiling:     rec.Ceiling,
		}
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &evt1))
		sched.fireEvent(context.Background(), evt1, false)
		assert.Equal(t, store.ScheduledEventPending, ms.getEvent(evt1.ID).Status, "admitted one-shot exec leaves event row to run tracker")

		admitted = false
		evt2 := evt1
		evt2.ID = "evt-oneshot-exec-skip"
		require.NoError(t, ms.CreateScheduledEvent(context.Background(), &evt2))
		sched.fireEvent(context.Background(), evt2, false)
		gotSkip := ms.getEvent(evt2.ID)
		assert.Equal(t, store.ScheduledEventSkipped, gotSkip.Status)
		assert.Equal(t, "not running", gotSkip.Error)

		fs.mu.Lock()
		assert.Equal(t, 0, fs.completeCalls)
		assert.Equal(t, 0, fs.unadmittedExecCalls)
		fs.mu.Unlock()
	})
}

func setFireCoreExperiments(t *testing.T, srv *Server, on map[string]bool) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"overrides": on})
	require.NoError(t, err)
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", raw)
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	_, err = ops.Refresh(context.Background())
	require.NoError(t, err)
	srv.SetOperationalSettings(ops)
}

type agentLookupErrStore struct {
	*mockScheduledEventStore
	err error
}

func (s *agentLookupErrStore) GetAgent(_ context.Context, _ string) (*store.Agent, error) {
	return nil, s.err
}

func TestFireCoreExecActionPreauthorizeAndAuthorize(t *testing.T) {
	// Not t.Parallel: captures process-default slog logger for audit assertions.
	const projectID = "project-exec-fireaction"

	setupFixture := func(t *testing.T) (*mockScheduledEventStore, *Server, hubfire.RecordedAuthority, FireAuthority, Identity, *store.Agent) {
		t.Helper()
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)
		setFireCoreExperiments(t, srv, map[string]bool{
			conduitExperiment:         true,
			experiments.ExecSchedules: true,
		})
		srv.registerFireActions()

		rec := newFireCoreTestAuthority(ms, projectID)
		rec.DefinitionKind = hubfire.DefinitionKindScheduledEvent
		rec.DefinitionID = "evt-exec-1"
		rec.FireID = "evt-exec-1"
		auth, identity, err := srv.resolveRecordedAuthority(context.Background(), rec, projectID, scheduledExecPermission)
		require.NoError(t, err)

		target := &store.Agent{
			ID:        "agent-exec-target",
			Slug:      "exec-worker",
			Name:      "exec-worker",
			ProjectID: projectID,
			OwnerID:   rec.Attribution.InitiatorPrincipalID,
			Phase:     "running",
			RunID:     "launch-1",
		}
		require.NoError(t, ms.CreateAgent(context.Background(), target))
		return ms, srv, rec, auth, identity, target
	}

	t.Run("Preauthorize skips flag-off and missed-window before authority resolution", func(t *testing.T) {
		logBuf := &fireCoreLogBuffer{}
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		_, srv, _, _, _, target := setupFixture(t)
		unrecorded := hubfire.RecordedAuthority{
			DefinitionKind: hubfire.DefinitionKindScheduledEvent,
			DefinitionID:   "evt-preauth",
			FireID:         "evt-preauth",
		}
		payload := `{"agentId":"` + target.ID + `","command":["true"]}`

		// 1. Flag off skips before authority resolution.
		setFireCoreExperiments(t, srv, map[string]bool{
			conduitExperiment:         true,
			experiments.ExecSchedules: false,
		})
		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  unrecorded,
			ActionType: scheduledEventTypeExec,
			Payload:    payload,
			FireAt:     time.Now(),
		})
		assert.Equal(t, hubfire.FireStatusSkipped, out.Status)
		assert.Equal(t, execReasonDisabled, out.Error)
		assert.Contains(t, logBuf.String(), "audit_action=schedule.exec.skipped")
		assert.Contains(t, logBuf.String(), "reason=experiment_off")

		// 2. Missed window skips before authority resolution.
		logBuf.mu.Lock()
		logBuf.buf.Reset()
		logBuf.mu.Unlock()
		setFireCoreExperiments(t, srv, map[string]bool{
			conduitExperiment:         true,
			experiments.ExecSchedules: true,
		})
		out = srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  unrecorded,
			ActionType: scheduledEventTypeExec,
			Payload:    payload,
			FireAt:     time.Now().Add(-scheduledExecMaxWindow - time.Minute),
		})
		assert.Equal(t, hubfire.FireStatusSkipped, out.Status)
		assert.Equal(t, execReasonMissedWindow, out.Error)
		assert.Contains(t, logBuf.String(), "audit_action=schedule.exec.skipped")
		assert.Contains(t, logBuf.String(), "reason=missed_window")
	})

	t.Run("Authorize refusal paths emit audit and return generic exec refused", func(t *testing.T) {
		logBuf := &fireCoreLogBuffer{}
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		ms, srv, rec, auth, identity, target := setupFixture(t)
		act := &execAction{srv: srv}
		baseReq := hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: scheduledEventTypeExec,
			Payload:    `{"agentId":"` + target.ID + `","agentName":"` + target.Slug + `","command":["true"]}`,
			FireAt:     time.Now(),
		}

		// 1. Missing target (never falls back to AgentName).
		missingReq := baseReq
		missingReq.Payload = `{"agentId":"missing-agent-id","agentName":"` + target.Slug + `","command":["true"]}`
		authorized, refusal, err := act.Authorize(context.Background(), missingReq, auth, identity)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)
		assert.Contains(t, logBuf.String(), "audit_action=schedule.exec.refused")

		// 2. Deleted target.
		deletedTarget := *target
		deletedTarget.ID = "agent-deleted"
		deletedTarget.DeletedAt = time.Now()
		require.NoError(t, ms.CreateAgent(context.Background(), &deletedTarget))
		delReq := baseReq
		delReq.Payload = `{"agentId":"` + deletedTarget.ID + `","command":["true"]}`
		authorized, refusal, err = act.Authorize(context.Background(), delReq, auth, identity)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)

		// 3. Cross-project target.
		foreignTarget := *target
		foreignTarget.ID = "agent-foreign-project"
		foreignTarget.ProjectID = "other-project"
		require.NoError(t, ms.CreateAgent(context.Background(), &foreignTarget))
		crossReq := baseReq
		crossReq.Payload = `{"agentId":"` + foreignTarget.ID + `","command":["true"]}`
		authorized, refusal, err = act.Authorize(context.Background(), crossReq, auth, identity)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)

		// 4. Target store lookup error fails closed with generic refusal text.
		errStore := &agentLookupErrStore{mockScheduledEventStore: ms, err: errors.New("secret-db-internal-detail")}
		errSrv := newEventHandlerTestServer(errStore)
		errAct := &execAction{srv: errSrv}
		authorized, refusal, err = errAct.Authorize(context.Background(), baseReq, auth, identity)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)
		assert.NotContains(t, refusal.Reason, "secret-db-internal-detail")

		// 5. Nil identity.
		authorized, refusal, err = act.Authorize(context.Background(), baseReq, auth, nil)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)

		// 6. Bounded ceiling without agent.attach.
		narrowAuth := auth
		narrowAuth.Ceiling = store.EffectCeiling{
			Kind:              store.EffectCeilingBounded,
			Version:           1,
			PermissionIDs:     []string{"agent.read", "agent.message"},
			BoundaryKind:      "project",
			BoundaryProjectID: projectID,
		}
		authorized, refusal, err = act.Authorize(context.Background(), baseReq, narrowAuth, identity)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)

		// 7. Live authorizeAgentTargetAction denial (unowned agent).
		unowned := *target
		unowned.ID = "agent-other-owner"
		unowned.OwnerID = "someone-else"
		require.NoError(t, ms.CreateAgent(context.Background(), &unowned))
		unownedReq := baseReq
		unownedReq.Payload = `{"agentId":"` + unowned.ID + `","command":["true"]}`
		authorized, refusal, err = act.Authorize(context.Background(), unownedReq, auth, identity)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)

		// 8. Held target (fireStanding denial).
		heldSrv := newEventHandlerTestServer(&heldAgentMockStore{
			mockScheduledEventStore: ms,
			heldID:                  target.ID,
		})
		heldAct := &execAction{srv: heldSrv}
		authorized, refusal, err = heldAct.Authorize(context.Background(), baseReq, auth, identity)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)
	})

	t.Run("Authorize preflight seam skips non-deliverable targets and returns resolved target when ready", func(t *testing.T) {
		ms, srv, rec, auth, identity, target := setupFixture(t)
		act := &execAction{srv: srv}

		preflightCases := []struct {
			name   string
			mutate func(a *store.Agent)
		}{
			{"managed runtime", func(a *store.Agent) { a.Runtime = "managed:gemini" }},
			{"stopped phase", func(a *store.Agent) { a.Phase = "stopped" }},
			{"suspended phase", func(a *store.Agent) { a.Phase = "suspended" }},
			{"starting phase", func(a *store.Agent) { a.Phase = "starting" }},
			{"reincarnating", func(a *store.Agent) { a.ReincarnationState = store.ReincarnationStateProvisioning }},
		}
		for i, tc := range preflightCases {
			t.Run(tc.name, func(t *testing.T) {
				cp := *target
				cp.ID = target.ID + "-" + string(rune('a'+i))
				tc.mutate(&cp)
				require.NoError(t, ms.CreateAgent(context.Background(), &cp))

				req := hubfire.FireRequest{
					ProjectID:  projectID,
					Authority:  rec,
					ActionType: scheduledEventTypeExec,
					Payload:    `{"agentId":"` + cp.ID + `","command":["true"]}`,
					FireAt:     time.Now(),
				}
				authorized, refusal, err := act.Authorize(context.Background(), req, auth, identity)
				assert.Nil(t, authorized)
				assert.Nil(t, refusal)
				reason, skipped := asScheduledSkip(err)
				require.True(t, skipped, "preflight must return skip sentinel at Authorize seam: %v", err)
				assert.Equal(t, execReasonTargetNotRunning, reason)

				out := srv.fireAction(context.Background(), req)
				assert.Equal(t, hubfire.FireStatusSkipped, out.Status)
				assert.Equal(t, execReasonTargetNotRunning, out.Error)
				assert.False(t, out.Detached)
			})
		}

		// Happy Authorize on a schedule fire returns the resolved deliverable target.
		okReq := hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  rec,
			ActionType: scheduledEventTypeExec,
			Payload:    `{"agentId":"` + target.ID + `","command":["echo","ok"],"timeoutSeconds":45}`,
			FireAt:     time.Now(),
		}
		authorized, refusal, err := act.Authorize(context.Background(), okReq, auth, identity)
		require.NoError(t, err)
		assert.Nil(t, refusal)
		ea, ok := authorized.(*execAuthorized)
		require.True(t, ok)
		assert.Equal(t, target.ID, ea.target.ID)
		assert.Equal(t, []string{"echo", "ok"}, ea.payload.Command)
		assert.Equal(t, 45, ea.payload.timeout())
	})

	t.Run("DefinitionKindTrigger fails closed with generic refusal before any skip or refusal audit", func(t *testing.T) {
		logBuf := &fireCoreLogBuffer{}
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prev) })

		ms, srv, _, auth, identity, target := setupFixture(t)
		act := &execAction{srv: srv}

		// Even with hub.exec_schedules disabled, stale FireAt, and unrecorded
		// authority, DefinitionKindTrigger refuses before Preauthorize's skip
		// audit or OnAuthorityRefused's refusal audit can fire.
		setFireCoreExperiments(t, srv, map[string]bool{
			conduitExperiment:         true,
			experiments.ExecSchedules: false,
		})
		trigReq := hubfire.FireRequest{
			ProjectID: projectID,
			Authority: hubfire.RecordedAuthority{
				DefinitionKind: hubfire.DefinitionKindTrigger,
				DefinitionID:   "trig-exec-1",
				FireID:         "fire-exec-1",
			},
			ActionType: scheduledEventTypeExec,
			Payload:    `{"agentId":"` + target.ID + `","command":["echo","ok"]}`,
			FireAt:     time.Now().Add(-scheduledExecMaxWindow - time.Hour),
		}

		out := srv.fireAction(context.Background(), trigReq)
		assert.Equal(t, hubfire.FireStatusRefused, out.Status)
		assert.Equal(t, execReasonRefused, out.Error)
		assert.Empty(t, out.ResultRef)
		assert.False(t, out.Detached)
		assert.NotContains(t, logBuf.String(), "audit_action=schedule.exec.")

		// Direct Authorize and Fire calls also fail closed with generic refusal
		// text and zero schedule.exec.* audit records.
		trigRec := newFireCoreTestAuthority(ms, projectID) // DefinitionKindTrigger
		authorized, refusal, err := act.Authorize(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  trigRec,
			ActionType: scheduledEventTypeExec,
			Payload:    trigReq.Payload,
			FireAt:     time.Now(),
		}, auth, identity)
		require.NoError(t, err)
		assert.Nil(t, authorized)
		require.NotNil(t, refusal)
		assert.Equal(t, execReasonRefused, refusal.Reason)

		fireOut, fireErr := act.Fire(context.Background(), trigReq, &execAuthorized{
			payload: &scheduledExecPayload{AgentID: target.ID, Command: []string{"echo", "ok"}},
			target:  target,
			auth:    auth,
		})
		require.EqualError(t, fireErr, execReasonRefused)
		assert.Empty(t, fireOut.ResultRef)
		assert.False(t, fireOut.Detached)
		assert.NotContains(t, logBuf.String(), "audit_action=schedule.exec.")
	})
}

// TestFireCoreTriggerGenericErrorText pins 1b-3 review N4 (Stage 1 §5.5):
// trigger fires map raw internal errors onto the generic set so agent slugs,
// broker HTTP bodies, and context errors never leak into trigger_fires.error,
// while schedule fires retain their existing scheduled_events.error text.
func TestFireCoreTriggerGenericErrorText(t *testing.T) {
	t.Parallel()

	const projectID = "proj-n4-generic"
	ms := newMockStore()
	srv := newEventHandlerTestServer(ms)
	rec := newFireCoreTestAuthority(ms, projectID)

	cases := []struct {
		name       string
		actionType string
		rawErr     string
		wantTrig   string
	}{
		{
			name:       "dispatch error with slug and deadline",
			actionType: "message",
			rawErr:     `failed to dispatch message to agent secret-slug: context deadline exceeded`,
			wantTrig:   "message delivery failed",
		},
		{
			name:       "target stopped phase message",
			actionType: "message",
			rawErr:     `agent "secret-slug" is not running (phase: stopped)`,
			wantTrig:   "target not running",
		},
		{
			name:       "reincarnation in flight",
			actionType: "message",
			rawErr:     "target agent is reincarnating",
			wantTrig:   "agent restarted during fire",
		},
		{
			name:       "wake error with slug",
			actionType: "message",
			rawErr:     `failed to wake agent secret-slug: 502: bad gateway`,
			wantTrig:   "target wake failed",
		},
		{
			name:       "hub shutting down",
			actionType: "message",
			rawErr:     errScheduledWakeShuttingDown.Error(),
			wantTrig:   "hub shutting down",
		},
		{
			name:       "designed env-gather remedy preserved",
			actionType: "dispatch_agent",
			rawErr:     "missing required environment variables (2)",
			wantTrig:   "missing required environment variables (2)",
		},
		{
			name:       "generic refusal preserved",
			actionType: "message",
			rawErr:     errScheduledMessageRefused.Error(),
			wantTrig:   errScheduledMessageRefused.Error(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv.RegisterAction(&fakeFireAction{
				actionType: tc.actionType,
				permission: scheduledMessagePermission,
				fireFn: func(context.Context, hubfire.FireRequest, FireAuthorized) (hubfire.FireOutcome, error) {
					return hubfire.FireOutcome{}, errors.New(tc.rawErr)
				},
			})

			trigRec := rec
			trigRec.DefinitionKind = hubfire.DefinitionKindTrigger
			trigRec.DefinitionID = "tr-1"
			trigRec.FireID = "fire-1"
			trigOut := srv.fireAction(context.Background(), hubfire.FireRequest{
				ProjectID:  projectID,
				Authority:  trigRec,
				ActionType: tc.actionType,
			})
			assert.Equal(t, hubfire.FireStatusFailed, trigOut.Status)
			assert.Equal(t, tc.wantTrig, trigOut.Error)
			assert.NotContains(t, trigOut.Error, "secret-slug")

			schedRec := rec
			schedRec.DefinitionKind = hubfire.DefinitionKindSchedule
			schedRec.DefinitionID = "sched-1"
			schedRec.FireID = "evt-1"
			schedOut := srv.fireAction(context.Background(), hubfire.FireRequest{
				ProjectID:  projectID,
				Authority:  schedRec,
				ActionType: tc.actionType,
			})
			assert.Equal(t, hubfire.FireStatusFailed, schedOut.Status)
			assert.Equal(t, tc.rawErr, schedOut.Error, "schedule error text must remain bit-identical")
		})
	}
}
