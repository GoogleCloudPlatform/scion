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
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/hubfire"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeFireAction struct {
	actionType  string
	permission  string
	validateFn  func(req hubfire.FireRequest) error
	authorizeFn func(ctx context.Context, req hubfire.FireRequest, auth FireAuthority, identity Identity) (FireAuthorized, *FireRefusal, error)
	fireFn      func(ctx context.Context, req hubfire.FireRequest, a FireAuthorized) (hubfire.FireOutcome, error)
}

func (a *fakeFireAction) Type() string       { return a.actionType }
func (a *fakeFireAction) Permission() string { return a.permission }

func (a *fakeFireAction) Validate(_ context.Context, _ Identity, _ string, payload string) (string, error) {
	return payload, nil
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

	t.Run("legacyAction maps skip Detached and AfterRecord directly", func(t *testing.T) {
		ms := newMockStore()
		srv := newEventHandlerTestServer(ms)

		afterRan := false
		srv.RegisterAction(newLegacyAction("legacy_detached", func(ctx context.Context, evt store.ScheduledEvent) error {
			assert.Equal(t, "fire-leg-1", evt.ID)
			afterScheduledRecord(ctx, func() { afterRan = true })
			return outcomeRecorded(nil)
		}))

		out := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  hubfire.RecordedAuthority{FireID: "fire-leg-1"},
			ActionType: "legacy_detached",
		})
		assert.Equal(t, hubfire.FireStatusFired, out.Status)
		assert.True(t, out.Detached)
		require.NotNil(t, out.AfterRecord)
		assert.False(t, afterRan)
		out.AfterRecord(context.Background())
		assert.True(t, afterRan)

		srv.RegisterAction(newLegacyAction("legacy_skip", func(context.Context, store.ScheduledEvent) error {
			return skipScheduledFire("executor offline")
		}))
		skipOut := srv.fireAction(context.Background(), hubfire.FireRequest{
			ProjectID:  projectID,
			Authority:  hubfire.RecordedAuthority{FireID: "fire-leg-2"},
			ActionType: "legacy_skip",
		})
		assert.Equal(t, hubfire.FireStatusSkipped, skipOut.Status)
		assert.Equal(t, "executor offline", skipOut.Error)
		assert.False(t, skipOut.Detached)
	})
}

func TestFireCorePayloadValidatedBeforeAuthority(t *testing.T) {
	t.Parallel()

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
}

func TestFireCoreTracker(t *testing.T) {
	t.Parallel()

	t.Run("Detach survives caller cancel and observes fire abort", func(t *testing.T) {
		srv := newEventHandlerTestServer(newMockStore())
		tracker := newScheduledFireTracker(srv, "evt-detach", nil)

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

	t.Run("Admit registers with scheduledWakeFires and refuses after stop", func(t *testing.T) {
		srv := newEventHandlerTestServer(newMockStore())
		tracker := newScheduledFireTracker(srv, "evt-admit", nil)

		admittedCtx, done, ok := tracker.Admit(context.Background())
		require.True(t, ok)
		assert.Len(t, srv.runningWakeFires.snapshot(), 1)
		done()
		assert.Empty(t, srv.runningWakeFires.snapshot())
		assert.Error(t, admittedCtx.Err())

		srv.scheduledWakeFires.stop(context.Background(), 0)
		_, _, ok = tracker.Admit(context.Background())
		assert.False(t, ok)
	})

	t.Run("Record executes write at most once", func(t *testing.T) {
		srv := newEventHandlerTestServer(newMockStore())
		tracker := newScheduledFireTracker(srv, "evt-record", nil)

		var calls atomic.Int32
		var wg sync.WaitGroup
		var wins atomic.Int32
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if tracker.Record(func() { calls.Add(1) }) {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		assert.Equal(t, int32(1), wins.Load())
		assert.Equal(t, int32(1), calls.Load())
	})
}

func TestFireCoreNeverCallsSourceEffectCeiling(t *testing.T) {
	t.Parallel()

	coreFiles := map[string][]string{
		"firecore_authority.go": {
			"resolveRecordedAuthority",
			"fireRevisionCeilingConsistent",
			"resolveFireUser",
			"resolveFireAgent",
			"fireStanding",
		},
		"firecore_tracker.go": {
			"newScheduledFireTracker",
		},
		"firecore_action.go": {
			"RegisterAction",
			"LookupAction",
			"fireAction",
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
