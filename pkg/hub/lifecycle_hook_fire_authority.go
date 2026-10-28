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
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/lifecyclehooks"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// HookFireAuthorizer decides, immediately before the executor mints a GCP
// access token for a lifecycle hook, whether the hook may still act as its
// execution identity. A nil error allows the mint; any error refuses it.
type HookFireAuthorizer interface {
	AuthorizeHookFire(ctx context.Context, hook *store.LifecycleHook, sa *store.GCPServiceAccount) error
}

// SurfaceHookExecutionIdentityFire names the fire-time actAs check in audit
// records, distinct from the write-time surface
// (lifecyclehooks.SurfaceHookExecutionIdentity).
const SurfaceHookExecutionIdentityFire = "hook-execution-identity-fire"

// hookWritePermission is the permission the hook write handlers require
// (handleAdminLifecycleHooks, handleAdminLifecycleHookByID).
const hookWritePermission = "hub.lifecycle_hooks.update"

// HookFireDenyCause classifies a fire-time refusal.
type HookFireDenyCause string

const (
	HookFireWriterMissing         HookFireDenyCause = "writer_missing"
	HookFireWriterNotFound        HookFireDenyCause = "writer_not_found"
	HookFireWriterLookupError     HookFireDenyCause = "writer_lookup_error"
	HookFireWriterNewerThanHook   HookFireDenyCause = "writer_newer_than_hook"
	HookFireWriterInactive        HookFireDenyCause = "writer_inactive"
	HookFireWriterLacksPermission HookFireDenyCause = "writer_lacks_permission"
	HookFireIdentityNotAdmissible HookFireDenyCause = "identity_not_admissible"
	HookFireWriterActAsDenied     HookFireDenyCause = "writer_actas_denied"
	HookFireAuthorizerUnavailable HookFireDenyCause = "authorizer_unavailable"
)

// HookFireDeniedError is the refusal returned by AuthorizeHookFire.
type HookFireDeniedError struct {
	Cause  HookFireDenyCause
	Reason string
}

func (e *HookFireDeniedError) Error() string {
	return fmt.Sprintf("hook writer no longer authorized (%s): %s", e.Cause, e.Reason)
}

func hookFireDeny(cause HookFireDenyCause, reason string) error {
	return &HookFireDeniedError{Cause: cause, Reason: reason}
}

// denyingHookFireAuthorizer refuses every mint. It stands in for a missing
// authorizer so that an executor built without one fails closed.
type denyingHookFireAuthorizer struct{}

func (denyingHookFireAuthorizer) AuthorizeHookFire(context.Context, *store.LifecycleHook, *store.GCPServiceAccount) error {
	return hookFireDeny(HookFireAuthorizerUnavailable, "no fire-time authorizer is configured")
}

// AuthorizeHookFire re-checks, at the moment of the mint, the authority that
// writing the hook required. Write time (createLifecycleHook,
// updateLifecycleHook) requires an interactive user holding
// hub.lifecycle_hooks.update, an admissible service account, and the
// writer's actAs on it under the hook identity check mode. Without this
// re-check, a hook keeps minting tokens after its writer is deleted,
// suspended, demoted or loses actAs.
//
// The writer is the hook's recorded CreatedBy, the creator's email, which
// is the only writer record a hook carries; update does not change it, so a
// hook re-saved by another admin is still checked against its creator. That
// is stricter than the write gate, never looser.
//
// Steps, in order; the first failure refuses:
//  1. CreatedBy is set (writer_missing);
//  2. it names a user row (writer_not_found, writer_lookup_error);
//  3. the user is active, is not a test identity that can no longer
//     authenticate, and is not the development user while development
//     authority is off (writer_inactive);
//  4. a live decision for that user allows hub.lifecycle_hooks.update on the
//     hub, the request requireWritePermission makes (writer_lacks_permission);
//  5. the account passes lifecyclehooks.ExecutionIdentityAdmissible, the
//     write-time rule (identity_not_admissible);
//  6. store.EvaluateActAs for the user on the account allows, under the
//     same hook identity check mode as write time (writer_actas_denied).
//
// Every lookup error refuses. The executor records the refusal in the hook
// execution audit, rate limited per hook (hookRefusalLimiter); the actAs
// record of step 6 is rate limited the same way when it is not an allow.
func (s *Server) AuthorizeHookFire(ctx context.Context, hook *store.LifecycleHook, sa *store.GCPServiceAccount) error {
	return s.authorizeHookFire(ctx, hook, sa)
}

func (s *Server) authorizeHookFire(ctx context.Context, hook *store.LifecycleHook, sa *store.GCPServiceAccount) error {
	if s == nil || s.store == nil || s.authzService == nil {
		return hookFireDeny(HookFireAuthorizerUnavailable, "authorization service not available")
	}
	if hook == nil || sa == nil {
		return hookFireDeny(HookFireAuthorizerUnavailable, "invalid fire-time authorization request")
	}

	// 1. The recorded writer.
	if hook.CreatedBy == "" {
		return hookFireDeny(HookFireWriterMissing, "hook has no recorded writer")
	}

	// 2. The writer's user row, loaded fresh.
	user, err := s.store.GetUserByEmail(ctx, hook.CreatedBy)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return hookFireDeny(HookFireWriterNotFound, "hook writer does not exist")
		}
		return hookFireDeny(HookFireWriterLookupError, "hook writer lookup failed")
	}
	if user == nil {
		return hookFireDeny(HookFireWriterLookupError, "hook writer lookup returned no user")
	}
	// A user row created after the hook cannot be the user who wrote it:
	// user delete is a hard delete, so the writer's email can be taken by a
	// new account (an invite or a first sign-in) after the writer is gone.
	// The hook records only the email, so this is the identity check
	// available without a writer ID on the record.
	if !hook.Created.IsZero() && user.Created.After(hook.Created) {
		return hookFireDeny(HookFireWriterNewerThanHook, "the user now holding the hook writer's email was created after the hook")
	}

	// 3. The writer is live.
	if user.Status != store.UserStatusActive {
		return hookFireDeny(HookFireWriterInactive, "hook writer is not active (status "+user.Status+")")
	}
	if testFixtureRejection(user, s.testIdentities.enabled, s.testIdentities.clock()) != "" {
		return hookFireDeny(HookFireWriterInactive, "hook writer can no longer authenticate")
	}
	if user.ID == DevUserID && !s.authzService.devLocalAuthorityEnabled() {
		return hookFireDeny(HookFireWriterInactive, reasonDevLocalDisabled)
	}

	// 4. The write authority, live.
	writer := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "")
	decision := s.authzService.Decide(maskAuthzInputs(ctx), AuthzRequest{
		Principal:  principalContextForIdentity(writer),
		Credential: credentialContextForIdentity(writer),
		Resource:   Resource{Type: "hub", ID: "hub"},
		Action:     Action("update"),
		Permission: hookWritePermission,
	})
	if !decision.Allowed {
		return hookFireDeny(HookFireWriterLacksPermission, "hook writer does not hold "+hookWritePermission)
	}

	// 5. The account is still admissible as this hook's identity.
	if errs := lifecyclehooks.ExecutionIdentityAdmissible(hook, sa); len(errs) > 0 {
		return hookFireDeny(HookFireIdentityNotAdmissible, errs[0].Message)
	}

	// 6. The writer's actAs on the account, under the hook identity check
	// mode: the disabled checker when the mode is off, as at write time.
	result, err := store.EvaluateActAs(ctx, store.ActAsGate{
		Checker: s.hookIdentityCheckerFor(),
		Caller:  store.Principal{Kind: store.PrincipalUser, ID: user.ID, Email: user.Email},
		Surface: SurfaceHookExecutionIdentityFire,
		Audit:   s.hookFireActAsAudit(hook.ID),
	}, sa)
	if err != nil {
		// EvaluateActAs has already forced the outcome to indeterminate.
		slog.WarnContext(ctx, "lifecycle hook fire-time actAs check failed",
			"hook_id", hook.ID, "sa_id", sa.ID, "error", err)
	}
	if result.Outcome != store.ActAsAllowed {
		return hookFireDeny(HookFireWriterActAsDenied,
			fmt.Sprintf("hook writer may not act as the service account (%s): %s", result.Outcome.String(), result.Reason))
	}
	return nil
}

// hookFireActAsAudit returns the actAs audit sink for one fire of hookID. An
// allow record passes through: an allowed fire is followed by its own
// execution audit record anyway. Any other record passes through the
// per-hook refusal limiter, so a frequently firing hook whose writer lost
// actAs does not write one actAs denial record per fire.
func (s *Server) hookFireActAsAudit(hookID string) store.SAAssignmentAuditSink {
	inner := store.SAAssignmentAuditSink(s.GetAuditLogger())
	if inner == nil {
		return nil
	}
	return &rateLimitedActAsAuditSink{inner: inner, hookID: hookID, limiter: s.hookFireActAsLimiter()}
}

func (s *Server) hookFireActAsLimiter() *hookRefusalLimiter {
	s.hookFireLimiterOnce.Do(func() {
		s.hookFireLimiter = newHookRefusalLimiter(hookRefusalAuditInterval, time.Now)
	})
	return s.hookFireLimiter
}

type rateLimitedActAsAuditSink struct {
	inner   store.SAAssignmentAuditSink
	hookID  string
	limiter *hookRefusalLimiter
}

func (r *rateLimitedActAsAuditSink) RecordSAAssignment(ctx context.Context, event *store.SAAssignmentEvent) error {
	if event == nil || event.Decision == nil || *event.Decision == store.ActAsAllowed {
		return r.inner.RecordSAAssignment(ctx, event)
	}
	record, suppressed := r.limiter.admit(r.hookID, "actas:"+event.Decision.String())
	if !record {
		return nil
	}
	if suppressed > 0 {
		copied := *event
		copied.Reason = fmt.Sprintf("%s (%d similar refusals for this hook not recorded)", event.Reason, suppressed)
		event = &copied
	}
	return r.inner.RecordSAAssignment(ctx, event)
}

// hookRefusalAuditInterval is the shortest gap between two audit records of
// the same refusal cause for the same hook. A refusal with a different
// cause than the last recorded one is recorded at once.
const hookRefusalAuditInterval = 10 * time.Minute

// hookRefusalLimiterMaxEntries bounds the limiter's memory; past it, entries
// older than the interval are pruned. The prune scans the whole map, so it
// runs at most once per interval (an entry cannot expire sooner). A scan
// keeps the entries recorded within the interval before it, and the next
// scan is an interval later, so at worst the map holds the hooks first
// refused within about two intervals.
const hookRefusalLimiterMaxEntries = 1024

// hookRefusalLimiter rate limits refusal audit records per hook. It is safe
// for concurrent use.
type hookRefusalLimiter struct {
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]*hookRefusalEntry
	// lastPrune is when the last prune scan ran (zero: never).
	lastPrune time.Time
}

type hookRefusalEntry struct {
	cause      string
	recorded   time.Time
	suppressed int
}

func newHookRefusalLimiter(interval time.Duration, now func() time.Time) *hookRefusalLimiter {
	if now == nil {
		now = time.Now
	}
	return &hookRefusalLimiter{interval: interval, now: now, entries: make(map[string]*hookRefusalEntry)}
}

// admit reports whether a refusal of hookID with cause should be recorded
// now, and how many refusals of that hook were suppressed since the last
// record (meaningful only when record is true).
func (l *hookRefusalLimiter) admit(hookID, cause string) (record bool, suppressed int) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[hookID]
	if ok && e.cause == cause && now.Sub(e.recorded) < l.interval {
		e.suppressed++
		return false, 0
	}
	if ok {
		suppressed = e.suppressed
	}
	if !ok && len(l.entries) >= hookRefusalLimiterMaxEntries && now.Sub(l.lastPrune) >= l.interval {
		l.lastPrune = now
		for id, old := range l.entries {
			if now.Sub(old.recorded) >= l.interval {
				delete(l.entries, id)
			}
		}
	}
	l.entries[hookID] = &hookRefusalEntry{cause: cause, recorded: now}
	return true, suppressed
}
