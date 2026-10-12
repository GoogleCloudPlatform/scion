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
	"bytes"
	"errors"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin tier A2 (ptone/scion#4240): the TestAdminPolicy classification is
// complete, an unclassified or stale entry fails, the initial deny set is
// pinned, and every legacy admin route and role-claim admin gate in pkg/hub
// is classified (go/ast census, like TestBypassCensus and the A1 census).
// No database: these tests compile in every build.

// Every permission, legacy admin route, settings section and key,
// file-live key and experiment of this binary is classified, and nothing
// classified is stale. TestAdminPolicyComplete (the G3 startup check)
// agrees.
func TestTestAdminPolicy_Complete(t *testing.T) {
	in := currentTestAdminPolicyInputs()
	assert.Empty(t, testAdminPolicyProblems(in))
	require.NoError(t, TestAdminPolicyComplete())

	// The inputs are read from the live vocabularies, not from the tables.
	assert.Len(t, in.Permissions, len(permissions.Registry))
	assert.Len(t, in.SettingsSections, len(opsettings.Registry))
	assert.Len(t, in.Experiments, len(experiments.Default().All()))
	assert.Len(t, in.FileLiveKeys, len(fileLiveKeys))
	assert.NotEmpty(t, in.LegacyRoutes)
	for _, id := range legacyAdminRouteIDs() {
		assert.Contains(t, in.LegacyRoutes, id)
	}
}

// A fake unclassified entry in each vocabulary is reported, and so is an
// entry classified but no longer present (stale) and an invalid value.
func TestTestAdminPolicy_FakeUnclassifiedEntryFails(t *testing.T) {
	base := currentTestAdminPolicyInputs()
	clone := func() testAdminPolicyInputs {
		c := base
		c.Permissions = append([]string(nil), base.Permissions...)
		c.LegacyRoutes = append([]string(nil), base.LegacyRoutes...)
		c.SettingsSections = append([]string(nil), base.SettingsSections...)
		c.SettingsKeys = append([]string(nil), base.SettingsKeys...)
		c.FileLiveKeys = append([]string(nil), base.FileLiveKeys...)
		c.Experiments = append([]string(nil), base.Experiments...)
		return c
	}
	cases := []struct {
		name string
		add  func(*testAdminPolicyInputs)
		want string
	}{
		{"permission", func(in *testAdminPolicyInputs) { in.Permissions = append(in.Permissions, "hub.fake_power.update") }, `permission "hub.fake_power.update" has no TestAdminPolicy classification`},
		{"legacy route", func(in *testAdminPolicyInputs) { in.LegacyRoutes = append(in.LegacyRoutes, "admin.fakeRoute") }, `legacy admin route "admin.fakeRoute" has no TestAdminPolicy classification`},
		{"settings section", func(in *testAdminPolicyInputs) { in.SettingsSections = append(in.SettingsSections, "fake_section") }, `settings section "fake_section" has no TestAdminPolicy classification`},
		{"settings key", func(in *testAdminPolicyInputs) { in.SettingsKeys = append(in.SettingsKeys, "server.hub.fake_key") }, `settings key "server.hub.fake_key" has no TestAdminPolicy classification`},
		{"file-live key", func(in *testAdminPolicyInputs) { in.FileLiveKeys = append(in.FileLiveKeys, "fake_live_key") }, `file-live key "fake_live_key" has no TestAdminPolicy classification`},
		{"experiment", func(in *testAdminPolicyInputs) { in.Experiments = append(in.Experiments, "hub.fake_experiment") }, `experiment "hub.fake_experiment" has no TestAdminPolicy classification`},
		{"stale permission", func(in *testAdminPolicyInputs) { in.Permissions = in.Permissions[1:] }, "(stale TestAdminPolicy entry)"},
		{"stale experiment", func(in *testAdminPolicyInputs) { in.Experiments = in.Experiments[1:] }, "(stale TestAdminPolicy entry)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := clone()
			tc.add(&in)
			problems := testAdminPolicyProblems(in)
			require.NotEmpty(t, problems)
			assert.Contains(t, strings.Join(problems, "\n"), tc.want)
		})
	}

	// An invalid classification value is reported.
	prev, had := testAdminExperimentPolicy[experiments.AgentEdit]
	testAdminExperimentPolicy[experiments.AgentEdit] = "maybe"
	t.Cleanup(func() {
		if had {
			testAdminExperimentPolicy[experiments.AgentEdit] = prev
		}
	})
	assert.Contains(t, strings.Join(testAdminPolicyProblems(clone()), "\n"), `has an invalid TestAdminPolicy "maybe"`)
	require.ErrorIs(t, TestAdminPolicyComplete(), ErrTestAdminPolicyIncomplete, "the G3 startup check sees the same gap")
}

// The initial deny set (admin tier design §3.6.1) is deny, every settings
// section and key, file-live key and experiment is deny, and only allow
// and deny are used.
func TestTestAdminPolicy_InitialDenySet(t *testing.T) {
	for _, id := range testAdminMustDenyPermissions {
		assert.Equal(t, testAdminPolicyDeny, testAdminPermissionPolicy[id], id)
		assert.True(t, testAdminPermissionDenied(id), id)
	}
	families := 0
	for _, p := range permissions.Registry {
		for _, prefix := range testAdminMustDenyPermissionPrefixes {
			if strings.HasPrefix(p.ID, prefix) {
				families++
				assert.Equal(t, testAdminPolicyDeny, testAdminPermissionPolicy[p.ID], p.ID)
			}
		}
	}
	assert.GreaterOrEqual(t, families, 4, "hub.federation.* and hub.github_app.* exist")
	assert.True(t, testAdminPermissionDenied("hub.not_a_permission"), "an unclassified permission is denied")
	for name, tbl := range map[string]map[string]testAdminPolicy{
		"settings section": testAdminSettingsSectionPolicy,
		"settings key":     testAdminSettingsKeyPolicy,
		"file-live key":    testAdminFileLiveKeyPolicy,
		"experiment":       testAdminExperimentPolicy,
	} {
		for k, p := range tbl {
			assert.Equal(t, testAdminPolicyDeny, p, "%s %s: test identities get no settings or experiment writes", name, k)
		}
	}
	for k, p := range testAdminPermissionPolicy {
		assert.Contains(t, []testAdminPolicy{testAdminPolicyAllow, testAdminPolicyDeny}, p, k)
	}
	// The settings key matcher denies every classified key, nested keys,
	// file-live keys, sections, request-layout paths and unknown keys.
	for _, key := range []string{"server.hub.admin_emails", "telemetry.cloud.tls.ca_file", "runtimes.docker.type", "server.log_level",
		"auto_inject_gcloud_adc", "maintenance", "hub.public_url", "federation.enabled", "not.a.key", ""} {
		assert.False(t, testAdminSettingsKeyAllowed(key), key)
	}
	for _, e := range experiments.Default().All() {
		assert.False(t, testAdminExperimentAllowed(e.Name), e.Name)
	}
	assert.False(t, testAdminExperimentAllowed("hub.unknown_experiment"))
}

// requireAdminAt sites: every handler call passes a literal site name, the
// names are exactly testAdminRequireAdminAtSites, and requireAdmin /
// requireAdminFor are called only by the route guard and by each other.
func TestTestAdminPolicy_LegacyRouteCensus(t *testing.T) {
	var sites []string
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			key := file + ":" + fn.Name.Name
			switch sel.Sel.Name {
			case "requireAdminAt":
				if key == "authorize.go:requireAdminFor" {
					return true
				}
				require.Len(t, call.Args, 4, key)
				lit, ok := call.Args[3].(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "%s: requireAdminAt needs a literal site name", key)
				site, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				sites = append(sites, site)
			case "requireAdminFor":
				assert.Equal(t, "authorize.go:requireAdmin", key, "requireAdminFor is called only by requireAdmin; handlers use requireAdminAt")
			case "requireAdmin":
				assert.Equal(t, "route_metadata.go:routeGuard", key, "requireAdmin is the route guard's fallback only")
			}
			return true
		})
	})
	sort.Strings(sites)
	want := append([]string(nil), testAdminRequireAdminAtSites...)
	sort.Strings(want)
	assert.Equal(t, want, sites)
}

// Role-claim admin gates: every pkg/hub function that calls
// IsUnscopedLocalPlatformAdmin, IsSystemAdmin or IsHubAdmin (outside their
// definitions), and every "role gate" of the A1
// role==admin census, is classified in testAdminRoleGatePolicy; no entry is
// stale; and each deny entry's enforcing function calls
// testAdminPolicyRefuses (or testAdminPolicyRefusesUser) with that key.
func TestTestAdminPolicy_RoleGateCensus(t *testing.T) {
	found := map[string]bool{}
	refusals := map[string]map[string]bool{} // function name -> keys it refuses
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			switch name {
			case "IsUnscopedLocalPlatformAdmin":
				found[file+":"+fn.Name.Name] = true
			case "IsSystemAdmin", "IsHubAdmin":
				// Binding-based system-admin checks outside decide; the
				// definitions in authz.go are not gates.
				if file != "authz.go" {
					found[file+":"+fn.Name.Name] = true
				}
			case "testAdminPolicyRefuses", "testAdminPolicyRefusesUser":
				lit, ok := call.Args[len(call.Args)-1].(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "%s:%s: %s needs a literal gate key", file, fn.Name.Name, name)
				key, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				if refusals[fn.Name.Name] == nil {
					refusals[fn.Name.Name] = map[string]bool{}
				}
				refusals[fn.Name.Name][key] = true
				_, classified := testAdminRoleGatePolicy[key]
				assert.True(t, classified, "%s:%s refuses unclassified gate %q", file, fn.Name.Name, key)
			}
			return true
		})
	})
	for key, class := range testAdminRoleAdminCensus {
		if strings.HasPrefix(class, "role gate") {
			found[key] = true
		}
	}
	require.NotEmpty(t, found)
	for key := range found {
		_, ok := testAdminRoleGatePolicy[key]
		assert.True(t, ok, "role-claim admin gate %s has no TestAdminPolicy classification", key)
	}
	for key, g := range testAdminRoleGatePolicy {
		assert.True(t, found[key], "stale TestAdminPolicy role gate %s: no such gate", key)
		assert.NotEmpty(t, g.Reason, key)
		if g.Policy != testAdminPolicyDeny {
			assert.Empty(t, g.EnforcedIn, "%s: only deny entries name an enforcing function", key)
			continue
		}
		fn := g.EnforcedIn
		if fn == "" {
			fn = key[strings.Index(key, ":")+1:]
		}
		assert.True(t, refusals[fn][key], "deny gate %s is not enforced: %s does not call testAdminPolicyRefuses(..., %q)", key, fn, key)
	}
}

// The sink chains every line to the previous one; the verifier accepts
// the chain (with unrelated output interleaved and across process starts)
// and detects a deleted, edited, inserted or reordered line.
func TestTestAdminAuditSink_ChainAndVerifier(t *testing.T) {
	var buf bytes.Buffer
	sink := newTestAdminAuditSink(&buf)
	for i := 0; i < 5; i++ {
		require.NoError(t, sink.emit("ev", map[string]any{"i": i}))
		buf.WriteString("unrelated process output\n")
	}
	n, err := verifyTestAdminAuditChain(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	// A second process start opens a new chain.
	second := newTestAdminAuditSink(&buf)
	require.NoError(t, second.emit("ev", nil))
	require.NoError(t, second.emit("ev", nil))
	n, err = verifyTestAdminAuditChain(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 7, n)

	tagged := func() []string {
		var out []string
		for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if strings.HasPrefix(l, testAdminAuditTag) {
				out = append(out, l)
			}
		}
		return out
	}
	join := func(lines []string) string { return strings.Join(lines, "\n") + "\n" }
	lines := tagged()
	require.Len(t, lines, 7)

	mutations := map[string][]string{
		"deleted line":   append(append([]string(nil), lines[:2]...), lines[3:]...),
		"deleted first":  lines[1:],
		"edited line":    append(append(append([]string(nil), lines[:2]...), strings.Replace(lines[2], `"i":2`, `"i":9`, 1)), lines[3:]...),
		"swapped lines":  append(append(append([]string(nil), lines[:1]...), lines[2], lines[1]), lines[3:]...),
		"inserted line":  append(append(append([]string(nil), lines[:3]...), lines[1]), lines[3:]...),
		"untagged chain": append(append(append([]string(nil), lines[:3]...), testAdminAuditTag+` {"event":"forged"}`), lines[3:]...),
	}
	for name, ls := range mutations {
		_, err := verifyTestAdminAuditChain(strings.NewReader(join(ls)))
		assert.ErrorIs(t, err, errTestAdminAuditChainBroken, name)
	}

	// A failed write does not advance the chain: the next line links to
	// the last line written.
	var ok bytes.Buffer
	w := &taFlakyWriter{out: &ok}
	s := newTestAdminAuditSink(w)
	require.NoError(t, s.emit("a", nil))
	w.fail = true
	require.Error(t, s.emit("b", nil))
	w.fail = false
	require.NoError(t, s.emit("c", nil))
	n, err = verifyTestAdminAuditChain(bytes.NewReader(ok.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// The sink sets event, ts, seq and prev itself; a caller field of the same
// name does not override them.
func TestTestAdminAuditSink_ReservedFields(t *testing.T) {
	var buf bytes.Buffer
	sink := newTestAdminAuditSink(&buf)
	require.NoError(t, sink.emit("real", map[string]any{"event": "forged", "seq": 99, "prev": "x", "ts": "y"}))
	line := strings.TrimSpace(buf.String())
	assert.Contains(t, line, `"event":"real"`)
	assert.Contains(t, line, `"seq":0`)
	assert.Contains(t, line, `"prev":"`+testAdminAuditChainSeed+`"`)
	assert.NotContains(t, line, `"forged"`)
	_, err := verifyTestAdminAuditChain(strings.NewReader(buf.String()))
	require.NoError(t, err)
	var nilSink *testAdminAuditSink
	require.Error(t, nilSink.emit("x", nil), "an unconfigured sink fails, so callers fail closed")
}

type taFlakyWriter struct {
	out  *bytes.Buffer
	fail bool
}

func (w *taFlakyWriter) Write(p []byte) (int, error) {
	if w.fail {
		return 0, errors.New("sink unavailable")
	}
	return w.out.Write(p)
}

// Review C1: CanDelegate runs the test-admin delegation check before the
// role-claim super-admin shortcut and before any type-specific
// non-amplification check, so neither admits a deny-classified grant by an
// admin kind.
func TestTestAdminPolicy_CanDelegateChecksPolicyFirst(t *testing.T) {
	var refusePos, shortcutPos token.Pos
	var typeChecks []token.Pos
	found := false
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		if file != "authz_candelegate.go" || fn.Name.Name != "CanDelegate" || fn.Recv == nil {
			return
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			switch {
			case name == "refuseTestAdminDeniedGrant" && refusePos == 0:
				refusePos = call.Pos()
			case name == "IsUnscopedLocalPlatformAdmin" && shortcutPos == 0:
				shortcutPos = call.Pos()
			case strings.HasPrefix(name, "canDelegate"):
				typeChecks = append(typeChecks, call.Pos())
			}
			return true
		})
	})
	require.True(t, found, "CanDelegate exists")
	require.NotZero(t, refusePos, "CanDelegate calls refuseTestAdminDeniedGrant")
	require.NotZero(t, shortcutPos)
	assert.Less(t, refusePos, shortcutPos, "the policy check runs before the super-admin shortcut")
	require.NotEmpty(t, typeChecks)
	for _, p := range typeChecks {
		assert.Less(t, refusePos, p, "the policy check runs before the type-specific checks")
	}
}
