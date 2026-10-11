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
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// Retiring artifacts and noticing deleted owners (ptone/scion#4529, the
// first phase of ptone/scion#4465).

// retireForbiddenMessage is the text of every 403 of the delete route: a
// caller that may read the artifact but not delete it. It states the rule,
// the same for every artifact and every reason, and never why this caller
// was refused.
const retireForbiddenMessage = "only the artifact's owner, a user with an admin grant on it, " +
	"or an admin of its home project may delete it"

func writeRetireForbidden(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "forbidden", retireForbiddenMessage)
}

// retireAuthority reports the authority by which the caller may delete a,
// an artifact it can read: AuthorityOwner or AuthorityAdminGrant (the
// administration rule, canAdminister), or AuthorityCustodianProjectAdmin
// when it administers a's current home project (ScopeAdministrator). Every
// authority needs a user whose credential permits artifact.manage in the
// home project. "" means it may not.
func (s *Service) retireAuthority(ctx context.Context, b backend, a *Artifact) (string, error) {
	authority, err := s.adminAuthorityWith(ctx, a, func() ([]Grant, error) { return b.store.ListGrants(ctx, a.ID) })
	if err != nil || authority != "" {
		return authority, err
	}
	kind, _, _, ok := s.host.Principal(ctx)
	if !ok || kind != PrincipalKindUser || !s.host.Permits(ctx, a.ScopeRef, PermissionManage) {
		return "", nil
	}
	if admin, ok := s.host.(ScopeAdministrator); ok && admin.AdministersScope(ctx, a.ScopeRef) {
		return AuthorityCustodianProjectAdmin, nil
	}
	return "", nil
}

// handleDeleteArtifact implements DELETE /{id}: a soft delete through
// Store.DeleteArtifact, which removes the artifact's grants and share
// links with it. The read check runs first, so a caller that cannot read
// the artifact gets the same 404 as for a missing one; a reader without
// authority (retireAuthority) gets 403 with one fixed body.
func (s *Service) handleDeleteArtifact(w http.ResponseWriter, r *http.Request, id string) {
	b, a, ok := s.readableArtifact(w, r, id)
	if !ok {
		return
	}
	ctx := r.Context()
	authority, err := s.retireAuthority(ctx, b, a)
	if err != nil {
		writeGrantsReadFailed(w, r, err)
		return
	}
	if authority == "" {
		writeRetireForbidden(w)
		return
	}
	switch _, err := b.store.DeleteArtifact(ctx, a.ID, DeleteOptions{Actor: s.callerActor(ctx, authority)}); {
	case errors.Is(err, ErrNotFound):
		writeNotFound(w)
	case err != nil:
		slog.ErrorContext(ctx, "artifacts: delete artifact failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal", "could not delete the artifact")
	default:
		w.Header().Set("Cache-Control", "private, no-store")
		w.WriteHeader(http.StatusNoContent)
	}
}

// ownerPageSize is how many distinct owners the reconciler asks the host
// about at once.
const ownerPageSize = 100

// orphanMarkBatch is how many artifacts of one owner one store call marks
// or clears.
const orphanMarkBatch = 500

// ReconcileResult reports one owner liveness pass.
type ReconcileResult struct {
	// Owners is how many distinct owners were checked.
	Owners int
	// Orphaned and Unorphaned count the artifacts marked and cleared.
	Orphaned   int
	Unorphaned int
}

// CheckerError is ReconcileOwners' error when the host's PrincipalChecker
// failed. The pass stopped at that page: the page and every owner after it
// were left unchanged, and Unprocessed counts them (-1 when counting
// failed too). Pages before it were applied.
type CheckerError struct {
	Unprocessed int
	Err         error
}

func (e *CheckerError) Error() string {
	return fmt.Sprintf("artifacts: principal check failed, %d owners left unprocessed: %v", e.Unprocessed, e.Err)
}

func (e *CheckerError) Unwrap() error { return e.Err }

// ReconcileOwners runs one owner liveness pass (design section 3.1): it
// pages through the distinct owners of live artifacts, asks checker about
// each page in one call, sets orphaned_at on the artifacts of owners that
// are gone and clears it on those of owners that are live again (a
// restored agent). It relies on no lifecycle hook, so it covers every way
// an agent or user stops existing, including the store-level purge and the
// project cascade, and it is idempotent. A nil checker does nothing.
//
// A checker error leaves the failing page and everything after it
// unchanged and returns a *CheckerError. A ref the checker does not answer
// counts as live.
func ReconcileOwners(ctx context.Context, st Store, checker PrincipalChecker, now time.Time) (ReconcileResult, error) {
	var res ReconcileResult
	if st == nil || checker == nil {
		return res, nil
	}
	var after *Owner
	for {
		page, err := st.ListLiveOwners(ctx, after, ownerPageSize)
		if err != nil {
			return res, err
		}
		if len(page) == 0 {
			return res, nil
		}
		refs := make([]string, len(page))
		for i, o := range page {
			refs[i] = PrincipalRef(o.Kind, o.Ref)
		}
		live, err := checker.PrincipalsExist(ctx, refs)
		if err != nil {
			unprocessed, cerr := st.CountLiveOwners(ctx, after)
			if cerr != nil {
				unprocessed = -1
			}
			return res, &CheckerError{Unprocessed: unprocessed, Err: err}
		}
		for i, o := range page {
			alive, answered := live[refs[i]]
			gone := answered && !alive
			switch {
			case gone && o.Unmarked > 0:
				n, err := markOwner(ctx, st, o, true, now)
				res.Orphaned += n
				if err != nil {
					return res, err
				}
			case !gone && o.Marked > 0:
				n, err := markOwner(ctx, st, o, false, now)
				res.Unorphaned += n
				if err != nil {
					return res, err
				}
			}
		}
		res.Owners += len(page)
		if len(page) < ownerPageSize {
			return res, nil
		}
		last := page[len(page)-1]
		after = &Owner{Kind: last.Kind, Ref: last.Ref}
	}
}

// markOwner sets or clears the orphan mark on every live artifact of o, a
// batch at a time.
func markOwner(ctx context.Context, st Store, o Owner, orphaned bool, now time.Time) (int, error) {
	total := 0
	for {
		n, err := st.SetOwnerOrphaned(ctx, o, orphaned, now, orphanMarkBatch)
		total += n
		if err != nil || n < orphanMarkBatch {
			return total, err
		}
	}
}
