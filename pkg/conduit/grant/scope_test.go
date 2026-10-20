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

package grant

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The scope claim of streams opened with a scoped token (design v2.21.31
// §3.5, Scope claim scope; criterion 29).

func testScope() *Scope {
	return NewScope([]string{"agent:port:access", "agent:attach", "agent:attach"},
		ScopeBoundary{Kind: ScopeBoundaryProject, ProjectID: "proj-1"}, 1, []string{"agent.port_access", "agent.attach"})
}

func TestNewScopeSortsAndDeduplicates(t *testing.T) {
	s := testScope()
	if strings.Join(s.Scopes, ",") != "agent:attach,agent:port:access" {
		t.Fatalf("scopes %v", s.Scopes)
	}
	if strings.Join(s.Ceiling.PermissionIDs, ",") != "agent.attach,agent.port_access" || s.Ceiling.Version != 1 {
		t.Fatalf("ceiling %+v", s.Ceiling)
	}
	if empty := NewScope([]string{"a"}, ScopeBoundary{Kind: ScopeBoundaryHub}, 0, nil); empty.Ceiling.PermissionIDs == nil {
		t.Fatal("an empty permission id list must be present, not null")
	}
}

func TestScopeValidate(t *testing.T) {
	if err := testScope().validate(); err != nil {
		t.Fatalf("valid scope: %v", err)
	}
	if err := NewScope([]string{"a"}, ScopeBoundary{Kind: ScopeBoundaryHub}, 0, []string{}).validate(); err != nil {
		t.Fatalf("hub boundary with an empty ceiling: %v", err)
	}
	for name, mut := range map[string]func(*Scope){
		"no scopes":                  func(s *Scope) { s.Scopes = nil },
		"unsorted scopes":            func(s *Scope) { s.Scopes = []string{"b", "a"} },
		"duplicate scopes":           func(s *Scope) { s.Scopes = []string{"a", "a"} },
		"project without project_id": func(s *Scope) { s.Boundary.ProjectID = "" },
		"hub with project_id":        func(s *Scope) { s.Boundary.Kind = ScopeBoundaryHub },
		"unknown boundary kind":      func(s *Scope) { s.Boundary.Kind = "org" },
		"negative ceiling version":   func(s *Scope) { s.Ceiling.Version = -1 },
		"null permission ids":        func(s *Scope) { s.Ceiling.PermissionIDs = nil },
		"unsorted permission ids":    func(s *Scope) { s.Ceiling.PermissionIDs = []string{"b", "a"} },
	} {
		t.Run(name, func(t *testing.T) {
			s := testScope()
			mut(s)
			if err := s.validate(); !errors.Is(err, ErrInvalidClaims) {
				t.Fatalf("err = %v, want ErrInvalidClaims", err)
			}
		})
	}
}

func TestScopeEqual(t *testing.T) {
	if !ScopeEqual(nil, nil) || ScopeEqual(testScope(), nil) || ScopeEqual(nil, testScope()) {
		t.Fatal("presence")
	}
	if !ScopeEqual(testScope(), testScope()) {
		t.Fatal("equal scopes")
	}
	for name, mut := range map[string]func(*Scope){
		"scopes":     func(s *Scope) { s.Scopes = []string{"agent:attach"} },
		"boundary":   func(s *Scope) { s.Boundary.ProjectID = "proj-2" },
		"kind":       func(s *Scope) { s.Boundary = ScopeBoundary{Kind: ScopeBoundaryHub} },
		"version":    func(s *Scope) { s.Ceiling.Version = 0 },
		"ceiling id": func(s *Scope) { s.Ceiling.PermissionIDs = []string{"agent.attach"} },
	} {
		other := testScope()
		mut(other)
		if ScopeEqual(testScope(), other) {
			t.Errorf("differs in %s but compares equal", name)
		}
	}
}

// TestGrantScopeClaim: scope round-trips on a stream grant, is omitted when
// nil, needs via, and a malformed scope cannot be minted.
func TestGrantScopeClaim(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	c := baseClaims()
	c.Via, c.Scope = ViaProxy, testScope()
	got, err := Verify(context.Background(), f.mint(t, c), f.keys, baseExpect(), newCache(), now)
	if err != nil || !ScopeEqual(got.Scope, testScope()) {
		t.Fatalf("scope round trip: %+v, %v", got, err)
	}
	unscoped := baseClaims()
	unscoped.Via = ViaProxy
	if pl := payloadOf(t, f.mint(t, unscoped)); pl["scope"] != nil {
		t.Fatal("a grant without scope carries the key")
	}
	noVia := baseClaims()
	noVia.Scope = testScope()
	if _, err := Mint(f.signer, noVia); err == nil {
		t.Fatal("scope without via was minted")
	}
	bad := c
	bad.Scope = &Scope{Scopes: []string{"b", "a"}, Boundary: ScopeBoundary{Kind: ScopeBoundaryHub}, Ceiling: ScopeCeiling{PermissionIDs: []string{}}}
	if _, err := Mint(f.signer, bad); err == nil {
		t.Fatal("a malformed scope was minted")
	}
}

// TestVerifyRenewal_ScopeMustMatch: a renewal's scope must equal the open
// grant's as a whole, including presence; a mismatch is refused without
// consuming the jti, and a renewal copying the scope is accepted.
func TestVerifyRenewal_ScopeMustMatch(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	other := testScope()
	other.Ceiling.PermissionIDs = []string{"agent.port_access"}
	for _, tc := range []struct {
		name          string
		open, renewal *Scope
		ok            bool
	}{
		{"both absent", nil, nil, true},
		{"both equal", testScope(), testScope(), true},
		{"renewal lacks the open grant's scope", testScope(), nil, false},
		{"renewal adds a scope", nil, testScope(), false},
		{"renewal has another scope", testScope(), other, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := baseRenewal()
			r.JTI, r.Scope = "renewal-"+tc.name, tc.renewal
			expect := baseRenewalExpect()
			expect.Scope = tc.open
			cache := newCache()
			_, err := VerifyRenewal(context.Background(), f.mintRenewal(t, r), f.keys, expect, cache, now)
			if tc.ok != (err == nil) {
				t.Fatalf("err = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok && (!errors.Is(err, ErrScope) || cache.Len() != 0) {
				t.Fatalf("err = %v (want ErrScope), jtis %d (want 0)", err, cache.Len())
			}
		})
	}
}

// TestVerifyRecheck_CarriesScope: a re-check returns the presented grant's
// scope, from an open grant and from a renewal.
func TestVerifyRecheck_CarriesScope(t *testing.T) {
	f := newFixture(t)
	now := t0.Add(time.Second)
	open := recheckOpenClaims()
	open.Scope = testScope()
	c, err := VerifyRecheck(f.mint(t, open), f.keys, recheckExpect(), now)
	if err != nil || !ScopeEqual(c.Scope, testScope()) {
		t.Fatalf("open grant: %+v, %v", c, err)
	}
	r := baseRenewal()
	r.Scope = testScope()
	c, err = VerifyRecheck(f.mintRenewal(t, r), f.keys, recheckExpect(), now)
	if err != nil || !ScopeEqual(c.Scope, testScope()) {
		t.Fatalf("renewal: %+v, %v", c, err)
	}
}

// TestMintRefusesOversizeToken: a grant or renewal that would exceed the
// size verifiers accept is not minted.
func TestMintRefusesOversizeToken(t *testing.T) {
	f := newFixture(t)
	scopes := make([]string, 0, 2000)
	for i := range 2000 {
		scopes = append(scopes, fmt.Sprintf("scope:%06d", i))
	}
	big := NewScope(scopes, ScopeBoundary{Kind: ScopeBoundaryHub}, 1, []string{})
	c := baseClaims()
	c.Via, c.Scope = ViaProxy, big
	if _, err := Mint(f.signer, c); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Mint: %v, want ErrTooLarge", err)
	}
	r := baseRenewal()
	r.Scope = big
	if _, err := MintRenewal(f.signer, r); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("MintRenewal: %v, want ErrTooLarge", err)
	}
}

// TestVerifyRecheck_MalformedScopeRefused: a grant whose scope is present
// but malformed gets no re-check, even when correctly signed.
func TestVerifyRecheck_MalformedScopeRefused(t *testing.T) {
	f := newFixture(t)
	open := recheckOpenClaims()
	open.Scope = testScope()
	tok := resignReplacing(t, f.signer.Key, f.mint(t, open),
		`"scopes":["agent:attach","agent:port:access"]`, `"scopes":["agent:port:access","agent:attach"]`)
	if _, err := VerifyRecheck(tok, f.keys, recheckExpect(), t0.Add(time.Second)); !errors.Is(err, ErrInvalidClaims) {
		t.Fatalf("err = %v, want ErrInvalidClaims", err)
	}
}
