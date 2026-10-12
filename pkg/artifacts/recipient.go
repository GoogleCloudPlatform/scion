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

package artifacts

import (
	"context"
	"errors"
	"slices"
	"time"
)

// Send-time recipient warning (ptone/scion#4575).
//
// When a message carries artifact references, the sender is told if a
// direct recipient may not be able to read them. Everything here is
// WARNING-ONLY: it never grants access, never serves content, and is never
// consulted by canReadWith or any route. A recipient still reads a
// reference only through ResolveRefs under its own credential.

// RecipientAuthority is an optional extension of Host for the send-time
// recipient warning. Unlike every Host method it answers for a named
// principal (kind, ref) rather than for the caller of ctx: the recipient of
// a message has no credential in the sender's request. It is consulted only
// by RecipientMayNotRead, whose answer is a count shown to the sender.
//
// Both methods fail closed (false) for an unknown kind, an unknown or
// inactive principal, an empty scope or an unknown permission. They return
// an error only when a lookup failed, so the caller can stay silent rather
// than warn on a failure.
type RecipientAuthority interface {
	// RecipientPermits reports whether a credential the principal would
	// hold next allows permission on artifacts homed in scopeRef: the
	// Permits question, asked of the credential the hub would issue. For
	// an agent, the scopes its next token would carry and its delegation
	// chain; for a user, true.
	RecipientPermits(ctx context.Context, kind, ref, scopeRef, permission string) (bool, error)

	// RecipientAuthorize reports whether host policy lets the principal
	// exercise permission on artifacts in scopeRef: the Authorize
	// question, without the credential step (RecipientPermits asks that).
	RecipientAuthorize(ctx context.Context, kind, ref, scopeRef, permission string) (bool, error)
}

// RecipientMayNotRead counts the references the principal (kind, ref) may
// not be able to read, for the sender's warning. It is warning-only (see
// above) and must be given only references the sender itself was just
// allowed to attach (admitted under the sender's own credential), so the
// count never tells a missing artifact apart from an unreadable one beyond
// what the sender's own read showed.
//
// It applies canReadWith's rules to the named principal, in the same
// order: expiry, the credential the principal would hold
// (RecipientPermits), owner, an unfinalized artifact (owner only), home
// scope policy (RecipientAuthorize), then an unexpired principal grant
// naming it or a scope grant for a scope it is authorized in. Share links
// never count.
//
// It returns 0 and no error when the host does not implement
// RecipientAuthority or there is no store. A lookup failure returns 0 and
// the error, for the caller to log and not warn: a failure is not evidence
// either way.
func (s *Service) RecipientMayNotRead(ctx context.Context, refs []MessageRef, kind, ref string) (int, error) {
	auth, ok := s.host.(RecipientAuthority)
	if !ok || len(refs) == 0 || kind == "" || ref == "" {
		return 0, nil
	}
	b, ok := s.backend()
	if !ok || b.store == nil {
		return 0, nil
	}
	readable := make(map[string]bool, len(refs))
	count := 0
	for _, r := range refs {
		can, done := readable[r.ArtifactID]
		if !done {
			var err error
			can, err = s.recipientCanRead(ctx, b, auth, r.ArtifactID, kind, ref)
			if err != nil {
				return 0, err
			}
			readable[r.ArtifactID] = can
		}
		if !can {
			count++
		}
	}
	return count, nil
}

// recipientCanRead is canReadWith for the named principal (kind, ref); see
// RecipientMayNotRead. An artifact that is gone counts as readable: it was
// admitted a moment ago, and the warning is about access, not existence.
func (s *Service) recipientCanRead(ctx context.Context, b backend, auth RecipientAuthority, id, kind, ref string) (bool, error) {
	if !canonicalID(id) {
		return true, nil
	}
	a, err := b.store.GetArtifact(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	now := time.Now()
	if a.ExpiresAt != nil && !now.Before(*a.ExpiresAt) {
		return false, nil
	}
	// 1. The credential the principal would hold.
	if ok, err := auth.RecipientPermits(ctx, kind, ref, a.ScopeRef, PermissionRead); err != nil || !ok {
		return false, err
	}
	// 2. Owner, or host policy in the home scope.
	if kind == a.OwnerKind && ref == a.OwnerRef {
		return true, nil
	}
	if a.CurrentSeq == 0 {
		return false, nil
	}
	if ok, err := auth.RecipientAuthorize(ctx, kind, ref, a.ScopeRef, PermissionRead); err != nil || ok {
		return ok, err
	}
	// 3. Grants.
	grants, err := b.store.ListGrants(ctx, a.ID)
	if err != nil {
		return false, err
	}
	for _, g := range grants {
		if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
			continue
		}
		if !slices.Contains(grantsForRead, g.Permission) {
			continue
		}
		switch g.SubjectKind {
		case SubjectPrincipal:
			if g.SubjectRef == PrincipalRef(kind, ref) {
				return true, nil
			}
		case SubjectScope:
			// The home scope was asked above (step 2).
			if g.SubjectRef == "" || g.SubjectRef == a.ScopeRef {
				continue
			}
			ok, err := auth.RecipientAuthorize(ctx, kind, ref, g.SubjectRef, PermissionRead)
			if err != nil {
				return false, err
			}
			if ok {
				return true, nil
			}
		}
	}
	return false, nil
}
