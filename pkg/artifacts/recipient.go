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
)

// Send-time recipient warning (ptone/scion#4575).
//
// When a message carries artifact references, the sender is told if a
// direct recipient may not be able to read them. Everything here is
// WARNING-ONLY: it never grants access, never serves content, and is never
// consulted by any route; it evaluates the one read rule, canReadWith, for
// a named recipient. A recipient still reads a reference only through
// ResolveRefs under its own credential.

// RecipientAuthority is an optional extension of Host for the send-time
// recipient warning. Unlike every Host method it answers for a named
// principal (kind, ref) rather than for the caller of ctx: the recipient of
// a message has no credential in the sender's request. It is consulted only
// by RecipientMayNotRead, whose answer is a count shown to the sender.
type RecipientAuthority interface {
	// ResolveRecipient looks the principal up once, for every question
	// one RecipientMayNotRead call asks about it. It returns a nil subject
	// and no error for an unknown kind or an unknown, deleted or inactive
	// principal (which can read nothing), and an error only when a lookup
	// failed.
	ResolveRecipient(ctx context.Context, kind, ref string) (RecipientSubject, error)
}

// RecipientSubject answers Host's two policy questions for a resolved
// recipient. Both fail closed (false) for an empty scope or an unknown
// permission, and return an error only when a lookup failed, so the caller
// can stay silent rather than warn on a failure.
type RecipientSubject interface {
	// Permits is Host.Permits asked of the credential the hub would issue
	// the principal next: for an agent, the scopes its next token would
	// carry and its delegation chain; for a user, true.
	Permits(ctx context.Context, scopeRef, permission string) (bool, error)

	// Authorize is host policy for the principal in scopeRef, without the
	// credential step (Permits asks that).
	Authorize(ctx context.Context, scopeRef, permission string) (bool, error)
}

// RecipientMayNotRead counts the references the principal (kind, ref) may
// not be able to read, for the sender's warning. It is warning-only (see
// above) and must be given only references the sender itself was just
// allowed to attach (admitted under the sender's own credential), so the
// count never tells a missing artifact apart from an unreadable one beyond
// what the sender's own read showed.
//
// The decision is canReadWith itself, evaluated with recipientHost, a Host
// whose caller is the named principal and whose Permits and Authorize are
// answered by its RecipientSubject: there is no second copy of the read
// rule. A reference to an artifact that is gone or has a malformed id is
// not counted (the warning is about access, not existence).
//
// It returns 0 and no error when the host does not implement
// RecipientAuthority or there is no store. Any lookup failure (the
// principal, the artifact, its grants, or a policy question whose answer
// needed a lookup that failed) returns 0 and the error, for the caller to
// log and not warn: a failure is not evidence either way.
func (s *Service) RecipientMayNotRead(ctx context.Context, refs []MessageRef, kind, ref string) (int, error) {
	auth, ok := s.host.(RecipientAuthority)
	if !ok || len(refs) == 0 || kind == "" || ref == "" {
		return 0, nil
	}
	b, ok := s.backend()
	if !ok || b.store == nil {
		return 0, nil
	}
	subject, err := auth.ResolveRecipient(ctx, kind, ref)
	if err != nil {
		return 0, err
	}
	h := &recipientHost{kind: kind, ref: ref, subject: subject}
	readable := make(map[string]bool, len(refs))
	count := 0
	for _, r := range refs {
		can, done := readable[r.ArtifactID]
		if !done {
			can, err = s.recipientCanRead(ctx, b, h, r.ArtifactID)
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

// recipientCanRead is canReadWith for the recipient h, with the artifact's
// grants loaded like canReadErr loads them. Any failure, of a lookup here
// or of a question h answered, is returned as the error.
func (s *Service) recipientCanRead(ctx context.Context, b backend, h *recipientHost, id string) (bool, error) {
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
	var loadErr error
	can := canReadWith(ctx, h, a, func() ([]Grant, error) {
		gs, err := b.store.ListGrants(ctx, a.ID)
		loadErr = err
		return gs, err
	})
	if loadErr != nil {
		return false, loadErr
	}
	if h.err != nil {
		return false, h.err
	}
	return can, nil
}

// recipientHost is the Host canReadWith sees for a recipient check: its
// caller is the named principal, Permits is the subject's Permits, and
// Authorize is Permits then the subject's Authorize, as the hub's Authorize
// starts with Permits. A nil subject permits nothing. The first error a
// question returns is kept (every later question answers false) and
// reported by recipientCanRead. Only Principal, Permits and Authorize are
// used by canReadWith; the list and cursor methods refuse.
type recipientHost struct {
	kind, ref string
	subject   RecipientSubject
	err       error
}

var _ Host = (*recipientHost)(nil)

func (h *recipientHost) Principal(context.Context) (kind, ref, homeScope string, ok bool) {
	return h.kind, h.ref, "", true
}

func (h *recipientHost) Permits(ctx context.Context, scopeRef, permission string) bool {
	return h.ask(ctx, h.subjectPermits, scopeRef, permission)
}

func (h *recipientHost) Authorize(ctx context.Context, scopeRef, permission string) bool {
	return h.Permits(ctx, scopeRef, permission) && h.ask(ctx, h.subjectAuthorize, scopeRef, permission)
}

func (h *recipientHost) subjectPermits(ctx context.Context, scopeRef, permission string) (bool, error) {
	return h.subject.Permits(ctx, scopeRef, permission)
}

func (h *recipientHost) subjectAuthorize(ctx context.Context, scopeRef, permission string) (bool, error) {
	return h.subject.Authorize(ctx, scopeRef, permission)
}

// ask answers one question through q, failing closed: false with no
// subject, after an earlier error, or on an error (which it keeps).
func (h *recipientHost) ask(ctx context.Context, q func(context.Context, string, string) (bool, error), scopeRef, permission string) bool {
	if h.subject == nil || h.err != nil {
		return false
	}
	ok, err := q(ctx, scopeRef, permission)
	if err != nil {
		h.err = err
		return false
	}
	return ok
}

var errRecipientHostNotServed = errors.New("artifacts: not served for a recipient check")

func (h *recipientHost) MemberScopes(context.Context) ([]string, error) {
	return nil, errRecipientHostNotServed
}

func (h *recipientHost) SealCursor(context.Context, string, string) (string, error) {
	return "", errRecipientHostNotServed
}

func (h *recipientHost) OpenCursor(context.Context, string, string) (string, error) {
	return "", errRecipientHostNotServed
}
