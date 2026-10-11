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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Creation-time test-identity provenance (ptone/scion#4240, Phase 3a).
//
// Every agent, project, schedule and group carries an immutable stamp
// (store.FixtureProvenance): the hub test identity at the root of the
// principal chain that created it, and that identity's issuer. This file
// is the only hub code that writes the stamp. Every create path calls one
// of the stamp* helpers below, which overwrite whatever the model carried
// with the value resolveFixtureRoot computes on the server, so no request
// field and no copied struct can supply it. TestFixtureProvenanceCensus
// keeps it that way.

// errFixtureRootUnresolved marks a create refused because the creating
// principal's provenance could not be resolved. A create never falls back
// to an empty stamp for a principal whose root is unknown. When the cause
// is a missing row the error also wraps store.ErrNotFound.
var errFixtureRootUnresolved = errors.New("the creating principal's provenance could not be resolved")

// fixturePrincipalKind values. They match the delegation principal kinds,
// so a scheduled authority's PrincipalKind can be used directly.
const (
	fixturePrincipalNone  = ""
	fixturePrincipalUser  = store.DelegationPrincipalUser
	fixturePrincipalAgent = store.DelegationPrincipalAgent
)

// fixturePrincipal is the principal a create runs as, as far as the stamp
// is concerned. The zero value is a system, broker, migration or other
// non-user, non-agent caller, which never yields a stamp.
type fixturePrincipal struct {
	Kind string
	ID   string
	// Email is the authenticated user's email, when known. It is used only
	// to refuse a create (a test identity whose row has disappeared), never
	// to grant a stamp.
	Email string
}

// fixturePrincipalFromContext returns the authenticated principal of ctx:
// the agent for an agent identity, the user for a user identity (session,
// access token or dev user), and the zero value otherwise.
func fixturePrincipalFromContext(ctx context.Context) fixturePrincipal {
	if a := GetAgentIdentityFromContext(ctx); a != nil {
		return fixturePrincipal{Kind: fixturePrincipalAgent, ID: a.ID()}
	}
	if u := GetUserIdentityFromContext(ctx); u != nil {
		return fixturePrincipal{Kind: fixturePrincipalUser, ID: u.ID(), Email: u.Email()}
	}
	return fixturePrincipal{}
}

// resolveFixtureRoot computes the stamp for a resource created by p,
// reading through st (the create's transaction where there is one):
//
//   - a user whose stored kind is test_fixture: (user ID, its issued_by). A
//     test identity without an issuer fails the create.
//   - any other user, or a user ID with no row: no stamp. The kind is read
//     from the store (lookupUserForKind, the read the test identity clamp
//     uses), never from token claims. The one exception refuses rather than
//     grants: a user ID with no row whose email is in the reserved test
//     identity domain is a test identity deleted after it authenticated, and
//     the create fails (only test identities are in that domain).
//   - an agent: the stamp of the agent's own row, so a sub-agent of a test
//     identity's agent, and its sub-agents, carry the same stamp. If the
//     agent row cannot be read the create fails; an agent caller never
//     gets an empty stamp by default.
//   - no principal (system, broker, migration): no stamp.
//
// Any store error other than a missing row is returned, and the create
// fails.
func resolveFixtureRoot(ctx context.Context, st store.Store, p fixturePrincipal) (store.FixtureProvenance, error) {
	switch p.Kind {
	case fixturePrincipalNone:
		return store.FixtureProvenance{}, nil
	case fixturePrincipalUser:
		u, found, err := lookupUserForKind(ctx, st, p.ID)
		if err != nil {
			return store.FixtureProvenance{}, fmt.Errorf("%w: user %s: %w", errFixtureRootUnresolved, p.ID, err)
		}
		if !found && isReservedTestIdentityEmail(p.Email) {
			return store.FixtureProvenance{}, fmt.Errorf("%w: test identity %s has no user row: %w", errFixtureRootUnresolved, p.ID, store.ErrNotFound)
		}
		if !found || !u.IsTestFixture() {
			return store.FixtureProvenance{}, nil
		}
		if u.IssuedBy == nil || *u.IssuedBy == "" {
			return store.FixtureProvenance{}, fmt.Errorf("%w: test identity %s has no issuer", errFixtureRootUnresolved, p.ID)
		}
		return store.FixtureProvenance{RootID: u.ID, IssuerID: *u.IssuedBy}, nil
	case fixturePrincipalAgent:
		if p.ID == "" {
			return store.FixtureProvenance{}, fmt.Errorf("%w: agent principal has no ID", errFixtureRootUnresolved)
		}
		parent, err := st.GetAgent(ctx, p.ID)
		if err != nil {
			return store.FixtureProvenance{}, fmt.Errorf("%w: creating agent %s: %w", errFixtureRootUnresolved, p.ID, err)
		}
		if !parent.Fixture.Valid() {
			return store.FixtureProvenance{}, fmt.Errorf("%w: creating agent %s has a partial stamp", errFixtureRootUnresolved, p.ID)
		}
		return parent.Fixture, nil
	default:
		return store.FixtureProvenance{}, fmt.Errorf("%w: unknown principal kind %q", errFixtureRootUnresolved, p.Kind)
	}
}

// stampAgent sets a's stamp from p. commitAgentCreate calls it inside the
// create transaction, for both the interactive and the scheduled create.
func stampAgent(ctx context.Context, st store.Store, a *store.Agent, p fixturePrincipal) error {
	stamp, err := resolveFixtureRoot(ctx, st, p)
	if err != nil {
		return err
	}
	a.Fixture = stamp
	return nil
}

// stampProject sets proj's stamp from p. createProjectWithOwner calls it
// inside the create transaction (create, register and clone).
func stampProject(ctx context.Context, st store.Store, proj *store.Project, p fixturePrincipal) error {
	stamp, err := resolveFixtureRoot(ctx, st, p)
	if err != nil {
		return err
	}
	proj.Fixture = stamp
	return nil
}

// stampGroup sets g's stamp from p, for a group created through the group
// API.
func stampGroup(ctx context.Context, st store.Store, g *store.Group, p fixturePrincipal) error {
	stamp, err := resolveFixtureRoot(ctx, st, p)
	if err != nil {
		return err
	}
	g.Fixture = stamp
	return nil
}

// stampProjectGroup sets the stamp of a project's implicit agents or
// members group to the project's own stamp. Those groups are created by
// the hub as part of the project, so they are rooted wherever the project
// is. For a project read back from the store this is the stored stamp.
func stampProjectGroup(g *store.Group, proj *store.Project) {
	g.Fixture = proj.Fixture
}

// stampSchedule sets sc's stamp from p.
func stampSchedule(ctx context.Context, st store.Store, sc *store.Schedule, p fixturePrincipal) error {
	stamp, err := resolveFixtureRoot(ctx, st, p)
	if err != nil {
		return err
	}
	sc.Fixture = stamp
	return nil
}

// writeFixtureRootError writes the answer for a create refused by
// resolveFixtureRoot and reports whether err was such a refusal: 409 when
// the creating identity's row no longer exists, 500 otherwise.
func writeFixtureRootError(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, errFixtureRootUnresolved) {
		return false
	}
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusConflict, ErrCodeConflict,
			"cannot create the resource: the identity creating it no longer exists", nil)
		return true
	}
	InternalError(w)
	return true
}
