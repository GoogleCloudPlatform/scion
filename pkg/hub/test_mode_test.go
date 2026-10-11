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

//go:build !no_sqlite && (!hubshard || hubshard_2)

package hub

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// C1: the parser is an exact four-string allowlist on the level; anything
// else resolves to off.
func TestParseTestMode(t *testing.T) {
	cases := []struct {
		in         string
		mode       TestMode
		hubID      string
		recognized bool
	}{
		{"off", TestModeOff, "", true},
		{"off:hub-1", TestModeOff, "", true},
		{"member", TestModeMember, "", true},
		{"member:hub-1", TestModeMember, "hub-1", true},
		{"admin:hub-1", TestModeAdmin, "hub-1", true},
		{"super-admin:hub-1", TestModeSuperAdmin, "hub-1", true},
		{"admin:", TestModeAdmin, "", true},
		{"admin:a:b", TestModeAdmin, "a:b", true},
		{"admin: hub-1", TestModeAdmin, " hub-1", true},
		// Fail closed: empty, unknown, case variants, whitespace, ordinals,
		// booleans, near misses.
		{"", TestModeOff, "", false},
		{":hub-1", TestModeOff, "", false},
		{"Member:hub-1", TestModeOff, "", false},
		{"MEMBER", TestModeOff, "", false},
		{"Admin:hub-1", TestModeOff, "", false},
		{"Super-Admin:hub-1", TestModeOff, "", false},
		{"OFF", TestModeOff, "", false},
		{" admin:hub-1", TestModeOff, "", false},
		{"admin :hub-1", TestModeOff, "", false},
		{"superadmin:hub-1", TestModeOff, "", false},
		{"super_admin:hub-1", TestModeOff, "", false},
		{"hub-admin:hub-1", TestModeOff, "", false},
		{"members:hub-1", TestModeOff, "", false},
		{"0", TestModeOff, "", false},
		{"1", TestModeOff, "", false},
		{"2", TestModeOff, "", false},
		{"3", TestModeOff, "", false},
		{"true", TestModeOff, "", false},
		{"on", TestModeOff, "", false},
		{"admin=hub-1", TestModeOff, "", false},
		{"admin;hub-1", TestModeOff, "", false},
		{"admin\x00:hub-1", TestModeOff, "", false},
	}
	for _, tc := range cases {
		mode, id, ok := ParseTestMode(tc.in)
		assert.Equal(t, tc.mode, mode, "%q", tc.in)
		assert.Equal(t, tc.hubID, id, "%q", tc.in)
		assert.Equal(t, tc.recognized, ok, "%q", tc.in)
	}
	assert.Equal(t, []TestMode{"off", "member", "admin", "super-admin"}, TestModeLevels())
}

func TestTestModeCapabilities(t *testing.T) {
	assert.Equal(t, TestModeCapabilities{}, TestModeOff.Capabilities())
	assert.Equal(t, TestModeCapabilities{TestIdentities: true}, TestModeMember.Capabilities())
	assert.Equal(t, TestModeCapabilities{TestIdentities: true, HubAdmin: true}, TestModeAdmin.Capabilities())
	assert.Equal(t, TestModeCapabilities{TestIdentities: true, HubAdmin: true, SuperAdmin: true}, TestModeSuperAdmin.Capabilities())
	assert.Equal(t, TestModeCapabilities{}, TestMode("Admin").Capabilities(), "an unknown level grants nothing")
	assert.Equal(t, TestModeCapabilities{}, TestMode("").Capabilities())
	assert.False(t, TestModeMember.IsAdminLevel())
	assert.True(t, TestModeAdmin.IsAdminLevel())
	assert.True(t, TestModeSuperAdmin.IsAdminLevel())
}

// New refuses any configuration whose switches differ from what the level
// grants.
func TestValidateTestModeConfig(t *testing.T) {
	cfg := func(level TestMode, ti, admin, super bool) ServerConfig {
		return ServerConfig{
			EnableTestIdentities: ti,
			TestAdminGate:        TestAdminGateConfig{HubAdmin: admin, SuperAdmin: super},
			TestMode:             TestModeConfig{Level: level},
		}
	}
	ok := []ServerConfig{
		cfg("", false, false, false),
		cfg(TestModeOff, false, false, false),
		cfg(TestModeMember, true, false, false),
		cfg(TestModeAdmin, true, true, false),
		cfg(TestModeSuperAdmin, true, true, true),
	}
	for _, c := range ok {
		assert.NoError(t, validateTestModeConfig(c), "%+v", c.TestMode)
	}
	bad := []ServerConfig{
		cfg(TestModeOff, true, false, false),
		cfg("", true, false, false),
		cfg(TestModeOff, true, true, false),
		cfg(TestModeMember, false, false, false),
		cfg(TestModeMember, true, true, false),
		cfg(TestModeAdmin, true, false, false),
		cfg(TestModeAdmin, false, true, false),
		cfg(TestModeSuperAdmin, true, true, false),
		cfg("Admin", true, true, false),
		cfg("bogus", false, false, false),
	}
	for _, c := range bad {
		err := validateTestModeConfig(c)
		assert.ErrorIs(t, err, ErrTestModeInconsistent, "%+v", c)
	}
}

// tmMemberServerConfig is a server config at --test-mode=member.
func tmMemberServerConfig() ServerConfig {
	cfg := testServerConfig()
	cfg.HubID = tiaHubID
	cfg.EnableTestIdentities = true
	cfg.TestMode = TestModeConfig{Level: TestModeMember, Preconditions: append([]string(nil), TestModeMemberPreconditions()...)}
	return cfg
}

func tmSinkLines(t *testing.T, out string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		require.True(t, strings.HasPrefix(line, testAdminAuditTag+" "), "tagged line: %q", line)
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(strings.TrimPrefix(line, testAdminAuditTag+" ")), &rec))
		recs = append(recs, rec)
	}
	return recs
}

func anyStrings(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// The member level writes one test_mode_start line with the level and the
// preconditions that admitted it; the status route keeps its three
// booleans; the health summary shows the level and preconditions.
func TestTestMode_MemberStartupLineAndStatus(t *testing.T) {
	buf := tiaCaptureAudit(t)
	srv, _, err := tiaNewServer(t, tmMemberServerConfig())
	require.NoError(t, err)
	assert.Equal(t, TestModeMember, srv.TestModeLevel())
	assert.True(t, srv.testIdentities.enabled)
	assert.False(t, srv.TestAdminTierOn())

	recs := tmSinkLines(t, buf.String())
	require.Len(t, recs, 1, "one startup line")
	assert.Equal(t, testAdminAuditEventTestModeStart, recs[0]["event"])
	assert.Equal(t, "member", recs[0]["test_mode"])
	assert.Equal(t, anyStrings(TestModeMemberPreconditions()), recs[0]["preconditions"])
	assert.Equal(t, tiaHubID, recs[0]["hub_id"])

	rec := doRequestNoAuth(t, srv, http.MethodGet, "/api/v1/test-infra/status", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, `{"testIdentities":true,"testHubAdmin":false,"testSuperAdmin":false}`, strings.TrimSpace(rec.Body.String()))

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "member", resp.Hub.TestMode)
	assert.Equal(t, TestModeMemberPreconditions(), resp.Hub.TestModePreconditions)
	assert.True(t, resp.Hub.TestIdentitiesEnabled)
	assert.False(t, resp.Hub.TestAdminIdentitiesEnabled)
	assert.False(t, resp.Hub.TestSuperAdminIdentitiesEnabled)
}

// The admin level writes one line (the gate-start banner) carrying the
// level and preconditions, signing_keys_hub_local included.
func TestTestMode_AdminStartupLineAndStatus(t *testing.T) {
	buf := tiaCaptureAudit(t)
	cfg := tiaGateServerConfig()
	cfg.TestMode.Preconditions = append(append([]string(nil), TestModeMemberPreconditions()...), TestModeAdminPreconditions()...)
	srv, _, err := tiaNewServer(t, cfg)
	require.NoError(t, err)
	assert.Equal(t, TestModeAdmin, srv.TestModeLevel())

	want := append(append(append([]string(nil), TestModeMemberPreconditions()...), TestModeAdminPreconditions()...), TestModePreconditionSigningKeysHubLocal)
	recs := tmSinkLines(t, buf.String())
	require.Len(t, recs, 1, "one startup line")
	assert.Equal(t, testAdminAuditEventGateStart, recs[0]["event"])
	assert.Equal(t, "admin", recs[0]["test_mode"])
	assert.Equal(t, anyStrings(want), recs[0]["preconditions"])

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp HealthSummaryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "admin", resp.Hub.TestMode)
	assert.Equal(t, want, resp.Hub.TestModePreconditions)
	assert.True(t, resp.Hub.TestIdentitiesEnabled)
	assert.True(t, resp.Hub.TestAdminIdentitiesEnabled)
}

// Off: no line, level "off", preconditions an empty list (never null).
func TestTestMode_OffStatus(t *testing.T) {
	buf := tiaCaptureAudit(t)
	srv, _, err := tiaNewServer(t, testServerConfig())
	require.NoError(t, err)
	assert.Equal(t, TestModeOff, srv.TestModeLevel())
	assert.Empty(t, buf.String())

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/admin/health/summary", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"test_mode":"off"`)
	assert.Contains(t, rec.Body.String(), `"test_mode_preconditions":[]`)
}

// New refuses a switch that the level does not grant, and a level whose
// switches are off, before writing any line.
func TestTestMode_NewRefusesInconsistentConfig(t *testing.T) {
	cases := map[string]func(*ServerConfig){
		"test identities without a level": func(c *ServerConfig) { c.TestMode = TestModeConfig{} },
		"member level without the switch": func(c *ServerConfig) { c.EnableTestIdentities = false },
		"unknown level":                   func(c *ServerConfig) { c.TestMode.Level = "Member" },
		"admin level without the gate":    func(c *ServerConfig) { c.TestMode.Level = TestModeAdmin },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			buf := tiaCaptureAudit(t)
			cfg := tmMemberServerConfig()
			mutate(&cfg)
			srv, _, err := tiaNewServer(t, cfg)
			require.Error(t, err)
			assert.Nil(t, srv)
			assert.True(t, errors.Is(err, ErrTestModeInconsistent), "got %v", err)
			assert.Empty(t, buf.String())
		})
	}
}

func TestTestMode_StartupLineWriteFailureRefusesStartup(t *testing.T) {
	prev := testAdminAuditOutput
	testAdminAuditOutput = tiaFailWriter{}
	t.Cleanup(func() { testAdminAuditOutput = prev })
	_, _, err := tiaNewServer(t, tmMemberServerConfig())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "startup line")
}

// The resolved level is immutable: unexported fields, no methods, assigned
// exactly once in production code, in New.
func TestTestMode_StateImmutable(t *testing.T) {
	st := reflect.TypeOf(testModeState{})
	for i := 0; i < st.NumField(); i++ {
		assert.False(t, st.Field(i).IsExported(), "testModeState field %s must be unexported", st.Field(i).Name)
	}
	assert.Equal(t, 0, reflect.PointerTo(st).NumMethod())

	assignments := 0
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(m ast.Node) bool {
				as, ok := m.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "testMode" {
						assignments++
						assert.Equal(t, "New", fn.Name.Name, "%s assigns testMode outside New", fset.Position(as.Pos()))
					}
				}
				return true
			})
		}
	}
	assert.Equal(t, 1, assignments, "testMode is assigned exactly once in production code")
}
