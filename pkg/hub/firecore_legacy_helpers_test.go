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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// messageEventHandler returns a thin test-compatibility EventHandler over
// messageAction so existing scheduler test suites calling
// srv.messageEventHandler() directly continue exercising the single
// fireActionWith path unchanged. Kept in *_helpers_test.go so hubshard compiles
// it in every shard while golangci-lint's --tests=false pass on ./pkg/hub sees
// no unused non-test symbol (hack/LINT-CONVENTIONS.md).
// Removed in PR 2 (ptone/scion#8029).
func (s *Server) messageEventHandler() EventHandler {
	return func(ctx context.Context, evt store.ScheduledEvent) error {
		exec, _ := ExecutorContextFromContext(ctx)
		req := scheduledFireRequest(s, evt, exec, nil)
		req.ActionType = "message"
		out, rawErr := s.fireActionWithRawError(withFireHistoryCreator(ctx, evt.CreatedBy), req, &messageAction{srv: s})
		if rawErr != nil && !out.Detached {
			return rawErr
		}
		return scheduledFireError("message", out)
	}
}

// execEventHandler returns a thin test-compatibility EventHandler over
// execAction so existing exec schedule test suites calling srv.execEventHandler()
// directly continue exercising the single fireActionWith path unchanged.
// Removed in PR 2c (ptone/scion#8029).
func (s *Server) execEventHandler() EventHandler {
	return func(ctx context.Context, evt store.ScheduledEvent) error {
		return s.fireScheduledExec(ctx, evt)
	}
}

// fireScheduledExec is a test-compatibility shim over execAction via
// fireActionWithRawError for existing exec test suites.
// Removed in PR 2c (ptone/scion#8029).
func (s *Server) fireScheduledExec(ctx context.Context, evt store.ScheduledEvent) error {
	exec, _ := ExecutorContextFromContext(ctx)
	req := scheduledFireRequest(s, evt, exec, nil)
	req.ActionType = scheduledEventTypeExec
	out, rawErr := s.fireActionWithRawError(withFireHistoryCreator(ctx, evt.CreatedBy), req, &execAction{srv: s})
	if out.AfterRecord != nil {
		after := out.AfterRecord
		afterScheduledRecord(ctx, func() {
			after(context.WithoutCancel(ctx))
		})
	}
	if rawErr != nil && !out.Detached {
		return rawErr
	}
	return scheduledFireError(scheduledEventTypeExec, out)
}

// authorizeScheduledExecFire is a test-compatibility shim over
// resolveScheduledAuthority + authorizeExecFire for existing exec authz tests.
// Removed in PR 2c (ptone/scion#8029).
func (s *Server) authorizeScheduledExecFire(ctx context.Context, evt store.ScheduledEvent, p *scheduledExecPayload) (ScheduledAuthority, Identity, *store.Agent, error) {
	auth, identity, err := s.resolveScheduledAuthority(withFireHistoryCreator(ctx, evt.CreatedBy), evt)
	if err != nil {
		return auth, nil, nil, err
	}
	target, err := s.authorizeExecFire(ctx, evt.ProjectID, auth, identity, p)
	return auth, identity, target, err
}
