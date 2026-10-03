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

package registry

import (
	"context"
	"time"
)

// Store operation names passed to FaultStore.Fault.
const (
	OpRegisterRelay                    = "RegisterRelay"
	OpHeartbeatRelay                   = "HeartbeatRelay"
	OpSetRelayDraining                 = "SetRelayDraining"
	OpDeleteSessionsOfOlderGenerations = "DeleteSessionsOfOlderGenerations"
	OpInsertSessionWithNextEpoch       = "InsertSessionWithNextEpoch"
	OpTouchSession                     = "TouchSession"
	OpSetSessionDraining               = "SetSessionDraining"
	OpDeleteSessionCAS                 = "DeleteSessionCAS"
	OpListPrincipalSessions            = "ListPrincipalSessions"
	OpListPrincipalSessionsBySession   = "ListPrincipalSessionsBySession"
	OpDeleteSessionsOfStaleRelays      = "DeleteSessionsOfStaleRelays"
	OpDeleteIdleRelays                 = "DeleteIdleRelays"
	OpDeletePrincipalEpoch             = "DeletePrincipalEpoch"
)

// FaultStore wraps a Store with a fault-injection hook for tests and the 1v
// validation environment. Before every operation it calls Fault (if set)
// with the operation name; a non-nil error is returned in place of calling
// Inner, exactly as a database error would be. Combine with Config.Clock to
// simulate stale relays, clock skew and unreadable authoritative state.
type FaultStore struct {
	Inner Store
	Fault func(ctx context.Context, op string) error
}

var _ Store = (*FaultStore)(nil)

func (f *FaultStore) fault(ctx context.Context, op string) error {
	if f.Fault == nil {
		return nil
	}
	return f.Fault(ctx, op)
}

// RegisterRelay implements Store.
func (f *FaultStore) RegisterRelay(ctx context.Context, r RelayInstance) (int64, error) {
	if err := f.fault(ctx, OpRegisterRelay); err != nil {
		return 0, err
	}
	return f.Inner.RegisterRelay(ctx, r)
}

// HeartbeatRelay implements Store.
func (f *FaultStore) HeartbeatRelay(ctx context.Context, instanceID string, gen int64, now time.Time) error {
	if err := f.fault(ctx, OpHeartbeatRelay); err != nil {
		return err
	}
	return f.Inner.HeartbeatRelay(ctx, instanceID, gen, now)
}

// SetRelayDraining implements Store.
func (f *FaultStore) SetRelayDraining(ctx context.Context, instanceID string, gen int64, draining bool) error {
	if err := f.fault(ctx, OpSetRelayDraining); err != nil {
		return err
	}
	return f.Inner.SetRelayDraining(ctx, instanceID, gen, draining)
}

// DeleteSessionsOfOlderGenerations implements Store.
func (f *FaultStore) DeleteSessionsOfOlderGenerations(ctx context.Context, instanceID string, currentGen int64) (int, error) {
	if err := f.fault(ctx, OpDeleteSessionsOfOlderGenerations); err != nil {
		return 0, err
	}
	return f.Inner.DeleteSessionsOfOlderGenerations(ctx, instanceID, currentGen)
}

// InsertSessionWithNextEpoch implements Store.
func (f *FaultStore) InsertSessionWithNextEpoch(ctx context.Context, s SessionRecord) (int64, error) {
	if err := f.fault(ctx, OpInsertSessionWithNextEpoch); err != nil {
		return 0, err
	}
	return f.Inner.InsertSessionWithNextEpoch(ctx, s)
}

// TouchSession implements Store.
func (f *FaultStore) TouchSession(ctx context.Context, sessionID string, now time.Time) error {
	if err := f.fault(ctx, OpTouchSession); err != nil {
		return err
	}
	return f.Inner.TouchSession(ctx, sessionID, now)
}

// SetSessionDraining implements Store.
func (f *FaultStore) SetSessionDraining(ctx context.Context, sessionID string) error {
	if err := f.fault(ctx, OpSetSessionDraining); err != nil {
		return err
	}
	return f.Inner.SetSessionDraining(ctx, sessionID)
}

// DeleteSessionCAS implements Store.
func (f *FaultStore) DeleteSessionCAS(ctx context.Context, sessionID, relayInstanceID string, relayGen int64) (bool, error) {
	if err := f.fault(ctx, OpDeleteSessionCAS); err != nil {
		return false, err
	}
	return f.Inner.DeleteSessionCAS(ctx, sessionID, relayInstanceID, relayGen)
}

// ListPrincipalSessions implements Store.
func (f *FaultStore) ListPrincipalSessions(ctx context.Context, principalKind, principalID string) (PrincipalSessions, error) {
	if err := f.fault(ctx, OpListPrincipalSessions); err != nil {
		return PrincipalSessions{}, err
	}
	return f.Inner.ListPrincipalSessions(ctx, principalKind, principalID)
}

// ListPrincipalSessionsBySession implements Store.
func (f *FaultStore) ListPrincipalSessionsBySession(ctx context.Context, sessionID string) (PrincipalSessions, bool, error) {
	if err := f.fault(ctx, OpListPrincipalSessionsBySession); err != nil {
		return PrincipalSessions{}, false, err
	}
	return f.Inner.ListPrincipalSessionsBySession(ctx, sessionID)
}

// DeleteSessionsOfStaleRelays implements Store.
func (f *FaultStore) DeleteSessionsOfStaleRelays(ctx context.Context, staleBefore time.Time) (int, error) {
	if err := f.fault(ctx, OpDeleteSessionsOfStaleRelays); err != nil {
		return 0, err
	}
	return f.Inner.DeleteSessionsOfStaleRelays(ctx, staleBefore)
}

// DeleteIdleRelays implements Store.
func (f *FaultStore) DeleteIdleRelays(ctx context.Context, staleBefore time.Time) (int, error) {
	if err := f.fault(ctx, OpDeleteIdleRelays); err != nil {
		return 0, err
	}
	return f.Inner.DeleteIdleRelays(ctx, staleBefore)
}

// DeletePrincipalEpoch implements Store.
func (f *FaultStore) DeletePrincipalEpoch(ctx context.Context, principalKind, principalID string) error {
	if err := f.fault(ctx, OpDeletePrincipalEpoch); err != nil {
		return err
	}
	return f.Inner.DeletePrincipalEpoch(ctx, principalKind, principalID)
}
