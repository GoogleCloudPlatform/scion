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

//go:build !no_sqlite

package hub

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authctx"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// revokeRelayRenewalUser calls the admin revoke-sessions endpoint for the
// relay renewal fixture's user and returns the new session generation.
func revokeRelayRenewalUser(t *testing.T, f *relayRenewalFixture) int64 {
	t.Helper()
	ctx := context.Background()
	before, err := f.store.GetUser(ctx, f.user.ID())
	require.NoError(t, err)
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/users/"+f.user.ID()+"/revoke-sessions", map[string]interface{}{})
	require.Equal(t, http.StatusOK, rec.Code, "revoke-sessions: %s", rec.Body.String())
	after, err := f.store.GetUser(ctx, f.user.ID())
	require.NoError(t, err)
	require.Equal(t, before.SessionGeneration+1, after.SessionGeneration)
	return after.SessionGeneration
}

// Fails if the session generation check is removed from the JWT bearer arm
// (the hub API and relay-admit lines) or from POST /api/v1/auth/refresh
// (the refresh line). A hub JWT session held by a separate relay: after
// revoke-sessions, the pre-revoke JWT is refused on the hub API and at
// relay admit, and its refresh token is refused at refresh. This test
// asserts nothing about sessions or streams already admitted.
func TestSessionGen_RelayHeldJWT_RevokedPathsRefused(t *testing.T) {
	f := newRelayRenewalFixture(t)
	_, refresh, _, err := f.srv.userTokenService.GenerateTokenPair(f.user.ID(), "renew@conduit.test", "Renew", store.UserRoleMember, ClientTypeCLI)
	require.NoError(t, err)

	revokeRelayRenewalUser(t, f)

	me := doRequestWithToken(t, f.srv, f.jwt, http.MethodGet, "/api/v1/auth/me", nil)
	assert.Equal(t, http.StatusUnauthorized, me.Code, "the pre-revoke JWT is refused on the hub API: %s", me.Body.String())

	_, err = f.ops.admit(t, bearer(f.jwt), "user-after-revoke")
	require.Error(t, err, "the pre-revoke JWT is refused at relay admit")
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err))

	rr := doRequestNoAuth(t, f.srv, http.MethodPost, "/api/v1/auth/refresh", AuthRefreshRequest{RefreshToken: refresh})
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "the pre-revoke refresh token is refused: %s", rr.Body.String())
}

// Fails if the session generation check is removed from the JWT bearer arm:
// relay admit replays the dialer's credential through that arm, and has no
// check of its own. A pre-revoke JWT and a JWT ahead of the row are
// refused; a JWT at the row's generation is admitted.
func TestSessionGen_ConduitRelayAdmit_RevokedJWT_Refused(t *testing.T) {
	f := newRelayRenewalFixture(t)
	row := revokeRelayRenewalUser(t, f)

	_, err := f.ops.admit(t, bearer(f.jwt), "user-revoked")
	require.Error(t, err, "the pre-revoke JWT is refused at relay admit")
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "pre-revoke JWT")

	ahead, _, _, err := f.srv.userTokenService.MintTokenPair(f.user.ID(), "renew@conduit.test", "Renew", store.UserRoleMember, row+1, ClientTypeWeb)
	require.NoError(t, err)
	_, err = f.ops.admit(t, bearer(ahead), "user-ahead")
	require.Error(t, err)
	assert.Equal(t, http.StatusUnauthorized, refusalStatus(t, err), "JWT ahead of the row")

	current, _, _, err := f.srv.userTokenService.MintTokenPair(f.user.ID(), "renew@conduit.test", "Renew", store.UserRoleMember, row, ClientTypeWeb)
	require.NoError(t, err)
	f.ops.mustAdmit(t, bearer(current), "user-current")
}

// Fails if the JWT bearer arm stops recording the session generation in the
// request context, or if relay admit stops handing writeRelayAdmission the
// request the middleware chain authenticated (for example if the admit
// context is re-rooted on the outer relay-peer request): the admission
// writer must see the users row's generation with ok=true.
func TestSessionGen_ConduitRelayAdmit_AuthctxVisibleInAdmission(t *testing.T) {
	f := newRelayRenewalFixture(t)
	revokeRelayRenewalUser(t, f)
	row := revokeRelayRenewalUser(t, f)

	type seen struct {
		gen int64
		ok  bool
	}
	var got []seen
	prev := conduitRelayAdmissionObserve
	conduitRelayAdmissionObserve = func(r *http.Request) {
		gen, ok := authctx.SessionGeneration(r.Context())
		got = append(got, seen{gen, ok})
	}
	t.Cleanup(func() { conduitRelayAdmissionObserve = prev })

	jwt, _, _, err := f.srv.userTokenService.MintTokenPair(f.user.ID(), "renew@conduit.test", "Renew", store.UserRoleMember, row, ClientTypeWeb)
	require.NoError(t, err)
	f.ops.mustAdmit(t, bearer(jwt), "user-authctx")
	require.Len(t, got, 1, "the admission writer ran once")
	assert.True(t, got[0].ok, "the admission writer sees a session generation")
	assert.Equal(t, row, got[0].gen, "the value is the users row's generation")
}

// Fails if a second non-test call to authctx.WithSessionGeneration is added in this module (nested modules and hidden directories,
// such as agent worktrees under .scion, are not scanned): the JWT bearer arm in pkg/hub/auth.go is the only place that may record a session generation.
func TestSessionGen_AuthctxSetterSingleCallSite(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "module root")

	var sites []string
	fset := token.NewFileSet()
	// pkgmove:scan-covers pkg/hub/...
	// recursive walk of the whole module: skips only dot, testdata, vendor and node_modules directories and nested modules, so it covers every package directory below pkg/hub.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			// Stay in this module's own sources: skip hidden directories
			// (.git, and .scion, which holds agent worktrees with full
			// copies of the tree), dependency and fixture directories, and
			// nested modules (extras/*).
			switch name := d.Name(); {
			case strings.HasPrefix(name, "."), name == "node_modules", name == "vendor", name == "testdata":
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(src), "WithSessionGeneration") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "WithSessionGeneration" {
				rel, _ := filepath.Rel(root, path)
				sites = append(sites, filepath.ToSlash(rel))
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"pkg/hub/auth.go"}, sites, "non-test calls of authctx.WithSessionGeneration")
}
