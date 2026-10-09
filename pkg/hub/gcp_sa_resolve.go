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
	"net/http"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// gcpSACandidate is one account in an identity_ambiguous response: enough
// for the caller to retry with an unambiguous reference (the id), and the
// scope that explains why more than one account matched.
type gcpSACandidate struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
}

// errGCPSAAmbiguous reports a service-account reference that matched more
// than one registered account. Every candidate is reachable from the
// project the reference was resolved in.
type errGCPSAAmbiguous struct {
	ref        string
	candidates []gcpSACandidate
}

func (e *errGCPSAAmbiguous) Error() string {
	parts := make([]string, 0, len(e.candidates))
	for _, c := range e.candidates {
		parts = append(parts, c.ID+" ("+c.Scope+")")
	}
	return fmt.Sprintf("GCP service account %q matches %d registered accounts: %s; use the account id",
		e.ref, len(e.candidates), strings.Join(parts, ", "))
}

// write renders the ambiguity as 400 identity_ambiguous, listing the
// candidates both in the message (what the CLI prints) and in details.
func (e *errGCPSAAmbiguous) write(w http.ResponseWriter) {
	writeError(w, http.StatusBadRequest, ErrCodeIdentityAmbiguous, e.Error(),
		map[string]interface{}{"reference": e.ref, "candidates": e.candidates})
}

// writeGCPSAAmbiguous writes the identity_ambiguous response when err is an
// ambiguity from resolveGCPServiceAccountRef and reports whether it did.
func writeGCPSAAmbiguous(w http.ResponseWriter, err error) bool {
	var amb *errGCPSAAmbiguous
	if errors.As(err, &amb) {
		amb.write(w)
		return true
	}
	return false
}

// resolveGCPServiceAccountRef resolves a caller-supplied service-account
// reference, as given to --service-account or a GCP identity default, in the
// context of projectID. It is the one resolver every assign site uses, so the
// forms a user may type are the same everywhere.
//
//   - A UUID matches the account id. The account is returned whatever its
//     scope; the caller's ReachableFromProject check decides admissibility,
//     exactly as before this resolver existed.
//   - A value containing "@" matches the email, ignoring case. The
//     project's own accounts are searched first and win over hub-scoped
//     ones (narrowest scope wins).
//   - Anything else matches the display name, across the project's accounts
//     and the hub-scoped ones together: a display name is not unique, so two
//     matches are always ambiguous, whatever their scopes.
//
// No match returns store.ErrNotFound, so callers keep answering with the
// single not-available message that does not reveal whether the account
// exists. More than one match returns *errGCPSAAmbiguous. Name and email
// lookups only ever see accounts reachable from projectID (the project's own
// and hub-scoped ones), so neither result discloses another project's
// accounts.
func (s *Server) resolveGCPServiceAccountRef(ctx context.Context, projectID, ref string) (*store.GCPServiceAccount, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, store.ErrNotFound
	}
	if _, err := uuid.Parse(ref); err == nil {
		return s.store.GetGCPServiceAccount(ctx, ref)
	}

	filter := store.GCPServiceAccountFilter{
		Scope:            store.ScopeProject,
		ScopeID:          projectID,
		IncludeHubScoped: true,
	}
	byEmail := strings.Contains(ref, "@")
	// One query for both scopes: the filter documents why the two halves
	// must not be read separately. The email is compared here rather than
	// through filter.Email, whose match is exact: registration does not
	// normalize case, and an address typed in a different case must still
	// match rather than read as "not available".
	all, err := s.store.ListGCPServiceAccounts(ctx, filter)
	if err != nil {
		return nil, err
	}

	var projectMatches, hubMatches []store.GCPServiceAccount
	for _, sa := range all {
		if !sa.ReachableFromProject(projectID) {
			continue
		}
		if byEmail {
			if !strings.EqualFold(sa.Email, ref) {
				continue
			}
		} else if sa.DisplayName != ref {
			continue
		}
		if sa.Scope == store.ScopeProject {
			projectMatches = append(projectMatches, sa)
		} else {
			hubMatches = append(hubMatches, sa)
		}
	}

	matches := append(projectMatches, hubMatches...)
	if byEmail && len(projectMatches) > 0 {
		matches = projectMatches
	}
	switch len(matches) {
	case 0:
		return nil, store.ErrNotFound
	case 1:
		sa := matches[0]
		return &sa, nil
	default:
		cands := make([]gcpSACandidate, 0, len(matches))
		for _, sa := range matches {
			cands = append(cands, gcpSACandidate{ID: sa.ID, Scope: sa.Scope})
		}
		sort.Slice(cands, func(i, j int) bool {
			if cands[i].Scope != cands[j].Scope {
				// Project-scoped first: the narrower scope reads first.
				return cands[i].Scope == store.ScopeProject
			}
			return cands[i].ID < cands[j].ID
		})
		return nil, &errGCPSAAmbiguous{ref: ref, candidates: cands}
	}
}
