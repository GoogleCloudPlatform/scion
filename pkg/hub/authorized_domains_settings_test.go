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
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/v2"
)

// Tests for ptone/scion#6000: a saved server.auth.authorized_domains is
// applied, a SCION_SERVER_* variable pins it on a node, an empty saved list
// falls back to the startup value, the startup drift warning, and the
// save-time check that a list does not refuse the saving account.

// newDomainsDBServer returns a DB-backed test hub whose startup authorized
// domains are startup. bootK and envK default to empty.
func newDomainsDBServer(t *testing.T, startup []string, bootK, envK *koanf.Koanf) (*Server, *fakeHubSettingStore, *OperationalSettings) {
	t.Helper()
	if bootK == nil {
		bootK = emptyKoanf()
	}
	if envK == nil {
		envK = emptyKoanf()
	}
	fakeStore := newFakeHubSettingStore()
	ops := NewOperationalSettings(fakeStore, bootK, envK)
	srv := &Server{dbDriver: "postgres", maintenance: NewMaintenanceState(false, "")}
	srv.config.AuthorizedDomains = startup
	srv.recordStartupLayer1Values()
	srv.SetOperationalSettings(ops)
	return srv, fakeStore, ops
}

// pinnedDomainsKoanfs returns a bootstrap and an env koanf in which
// SCION_SERVER_AUTH_AUTHORIZEDDOMAINS sets the list (the env layer is the
// top of the bootstrap merge).
func pinnedDomainsKoanfs(t *testing.T, domains ...string) (bootK, envK *koanf.Koanf) {
	t.Helper()
	v := make([]any, len(domains))
	for i, d := range domains {
		v[i] = d
	}
	return newFileKoanf(t, map[string]any{authorizedDomainsKey: v}), newEnvKoanf(t, map[string]any{authorizedDomainsKey: v})
}

func saveAccess(t *testing.T, ops *OperationalSettings, doc string) {
	t.Helper()
	if _, err := ops.Update(context.Background(), "access", json.RawMessage(doc), "test", -1, "managed"); err != nil {
		t.Fatalf("saving access %s: %v", doc, err)
	}
}

func applyOps(srv *Server, ops *OperationalSettings) map[string]interface{} {
	return ApplySnapshot(srv, ops.Snapshot())
}

func appliedKeys(res map[string]interface{}) []string {
	keys, _ := res["applied"].([]string)
	return keys
}

func TestAuthorizedDomainsForSnapshot(t *testing.T) {
	startup := []string{"startup.example"}
	pinned := []string{authorizedDomainsKey}
	for _, tc := range []struct {
		name string
		snap Layer1Snapshot
		want []string
	}{
		{"db saved list applies", Layer1Snapshot{FromOperationalSettings: true, AuthorizedDomains: []string{"saved.example"}}, []string{"saved.example"}},
		{"db empty list uses startup", Layer1Snapshot{FromOperationalSettings: true}, startup},
		{"db explicit empty list uses startup", Layer1Snapshot{FromOperationalSettings: true, AuthorizedDomains: []string{}}, startup},
		{"db env pin keeps startup", Layer1Snapshot{FromOperationalSettings: true, AuthorizedDomains: []string{"saved.example"}, EnvOverrides: pinned}, startup},
		{"other env override does not pin", Layer1Snapshot{FromOperationalSettings: true, AuthorizedDomains: []string{"saved.example"}, EnvOverrides: []string{"server.hub.admin_emails"}}, []string{"saved.example"}},
		{"file list applies", Layer1Snapshot{AuthorizedDomains: []string{"file.example"}}, []string{"file.example"}},
		{"file empty list applies as loaded", Layer1Snapshot{}, nil},
		// A blank entry is kept: dropping it would turn a list that
		// matches nothing into an empty one, which allows every domain.
		{"blank entry kept", Layer1Snapshot{FromOperationalSettings: true, AuthorizedDomains: []string{" "}}, []string{" "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := authorizedDomainsForSnapshot(tc.snap, startup)
			if !slices.Equal(got, tc.want) {
				t.Errorf("authorizedDomainsForSnapshot = %v, want %v", got, tc.want)
			}
		})
	}
}

// A saved list is applied to the running hub and enforced at sign-in; a row
// without a list returns to the startup value.
func TestApplySnapshot_AuthorizedDomainsSavedAppliesLive(t *testing.T) {
	srv, _, ops := newDomainsDBServer(t, []string{"startup.example"}, nil, nil)
	ctx := context.Background()

	applyOps(srv, ops)
	if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"startup.example"}) {
		t.Fatalf("no row: running = %v, want the startup list", got)
	}

	saveAccess(t, ops, `{"authorized_domains":["saved.example"]}`)
	res := applyOps(srv, ops)
	if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"saved.example"}) {
		t.Fatalf("saved: running = %v, want [saved.example]", got)
	}
	if !slices.Contains(appliedKeys(res), "authorized_domains") {
		t.Errorf("applied = %v, want authorized_domains", appliedKeys(res))
	}
	if d := checkSignInDenial(ctx, "user@startup.example", srv.AuthorizedDomains(), nil, "", nil); d != signInDeniedDomain {
		t.Errorf("sign-in from a domain only the startup list allows: denial %q, want %q", d, signInDeniedDomain)
	}
	if d := checkSignInDenial(ctx, "user@saved.example", srv.AuthorizedDomains(), nil, "", nil); d != "" {
		t.Errorf("sign-in from the saved domain: denial %q, want allowed", d)
	}

	// Applying the same snapshot again reports no change.
	if res := applyOps(srv, ops); slices.Contains(appliedKeys(res), "authorized_domains") {
		t.Errorf("unchanged apply reported authorized_domains: %v", appliedKeys(res))
	}

	// A row without the key (a cleared list is stored that way) returns to
	// the startup list rather than allowing every domain.
	saveAccess(t, ops, `{"admin_emails":["a@saved.example"]}`)
	applyOps(srv, ops)
	if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"startup.example"}) {
		t.Errorf("cleared: running = %v, want the startup list", got)
	}
}

// A SCION_SERVER_* variable pins the list on the node: the saved list is
// stored but the hub keeps the startup (environment) value.
func TestApplySnapshot_AuthorizedDomainsEnvPinWins(t *testing.T) {
	bootK, envK := pinnedDomainsKoanfs(t, "env.example")
	srv, _, ops := newDomainsDBServer(t, []string{"env.example"}, bootK, envK)

	saveAccess(t, ops, `{"authorized_domains":["saved.example"]}`)
	res := applyOps(srv, ops)
	if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"env.example"}) {
		t.Fatalf("running = %v, want the pinned [env.example]", got)
	}
	if slices.Contains(appliedKeys(res), "authorized_domains") {
		t.Errorf("applied = %v, the pinned list did not change", appliedKeys(res))
	}

	// The save report lists the key as skipped, with a reason that names
	// no value.
	skips := srv.layer1ApplySkips()
	if !skips.AuthorizedDomainsPinned {
		t.Fatal("layer1ApplySkips: AuthorizedDomainsPinned = false, want true")
	}
	r := savedKeysReport{Applied: []string{authorizedDomainsKey, "server.hub.admin_emails"}}.withSkipped(skips)
	if slices.Contains(r.Applied, authorizedDomainsKey) || !slices.Contains(r.Applied, "server.hub.admin_emails") {
		t.Errorf("report applied = %v", r.Applied)
	}
	if len(r.Skipped) != 1 || r.Skipped[0] != (skippedKey{Key: authorizedDomainsKey, Reason: authorizedDomainsPinnedReason}) {
		t.Errorf("report skipped = %+v", r.Skipped)
	}
}

// Without a pin, the report keeps the key under applied.
func TestSavedKeysReport_AuthorizedDomainsAppliedWithoutPin(t *testing.T) {
	srv, _, _ := newDomainsDBServer(t, nil, nil, nil)
	r := savedKeysReport{Applied: []string{authorizedDomainsKey}}.withSkipped(srv.layer1ApplySkips())
	if !slices.Equal(r.Applied, []string{authorizedDomainsKey}) || len(r.Skipped) != 0 {
		t.Errorf("report = %+v, want authorized_domains applied", r)
	}
}

// A file snapshot (file mode reload) applies the loaded list.
func TestApplySnapshot_AuthorizedDomainsFileSnapshot(t *testing.T) {
	srv := &Server{maintenance: NewMaintenanceState(false, "")}
	srv.config.AuthorizedDomains = []string{"startup.example"}
	srv.recordStartupLayer1Values()

	gc := &config.GlobalConfig{}
	gc.Auth.AuthorizedDomains = []string{"file.example"}
	ApplySnapshot(srv, BuildLayer1SnapshotFromFile(gc))
	if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"file.example"}) {
		t.Errorf("running = %v, want [file.example]", got)
	}
}

// captureAuthorizedDomainsWarnLogs routes slog to a buffer for the test.
func captureAuthorizedDomainsWarnLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestWarnAuthorizedDomainsDrift(t *testing.T) {
	const msg = "authorized_domains: the saved value differs from the deployment value"
	t.Run("saved differs", func(t *testing.T) {
		srv, _, ops := newDomainsDBServer(t, []string{"startup.example"}, nil, nil)
		saveAccess(t, ops, `{"authorized_domains":["saved.example","second.example"]}`)
		logs := captureAuthorizedDomainsWarnLogs(t)
		srv.WarnAuthorizedDomainsDrift(ops.Snapshot())
		out := logs.String()
		if !strings.Contains(out, msg) || !strings.Contains(out, "in_effect=saved") ||
			!strings.Contains(out, "saved_count=2") || !strings.Contains(out, "deployment_count=1") {
			t.Errorf("warning = %q", out)
		}
		for _, v := range []string{"saved.example", "second.example", "startup.example"} {
			if strings.Contains(out, v) {
				t.Errorf("warning names the domain %q: %q", v, out)
			}
		}
	})
	t.Run("pinned", func(t *testing.T) {
		bootK, envK := pinnedDomainsKoanfs(t, "env.example")
		srv, _, ops := newDomainsDBServer(t, []string{"env.example"}, bootK, envK)
		saveAccess(t, ops, `{"authorized_domains":["saved.example"]}`)
		logs := captureAuthorizedDomainsWarnLogs(t)
		srv.WarnAuthorizedDomainsDrift(ops.Snapshot())
		if out := logs.String(); !strings.Contains(out, msg) || !strings.Contains(out, "in_effect=deployment_pin") {
			t.Errorf("warning = %q", out)
		}
	})
	for _, tc := range []struct {
		name, doc string
	}{
		{"same set in another order and case", `{"authorized_domains":["B.example","a.example"]}`},
		{"no saved list", `{"admin_emails":["a@a.example"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, ops := newDomainsDBServer(t, []string{"a.example", "b.example"}, nil, nil)
			saveAccess(t, ops, tc.doc)
			logs := captureAuthorizedDomainsWarnLogs(t)
			srv.WarnAuthorizedDomainsDrift(ops.Snapshot())
			if out := logs.String(); out != "" {
				t.Errorf("unexpected warning: %q", out)
			}
		})
	}
	t.Run("file snapshot", func(t *testing.T) {
		srv := &Server{}
		srv.config.AuthorizedDomains = []string{"a.example"}
		srv.recordStartupLayer1Values()
		logs := captureAuthorizedDomainsWarnLogs(t)
		srv.WarnAuthorizedDomainsDrift(Layer1Snapshot{AuthorizedDomains: []string{"b.example"}})
		if out := logs.String(); out != "" {
			t.Errorf("unexpected warning: %q", out)
		}
	})
}

func TestAuthorizedDomainsLockout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		email   string
		domains []string
		admins  []string
		want    bool
	}{
		{"outside the list", "admin@example.com", []string{"other.example"}, nil, true},
		{"domain matches", "admin@example.com", []string{"other.example", "example.com"}, nil, false},
		{"domain matches case-insensitively", "Admin@Example.COM", []string{"EXAMPLE.com"}, nil, false},
		{"wildcard matches a subdomain", "admin@corp.example.com", []string{"*.example.com"}, nil, false},
		{"wildcard does not match the apex", "admin@example.com", []string{"*.example.com"}, nil, true},
		{"admin email allows", "admin@example.com", []string{"other.example"}, []string{" ADMIN@example.com "}, false},
		{"another admin does not allow", "admin@example.com", []string{"other.example"}, []string{"root@example.com"}, true},
		{"empty list allows every domain", "admin@example.com", nil, nil, false},
		{"no email is not refused", "", []string{"other.example"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := authorizedDomainsLockout(tc.email, tc.domains, tc.admins); got != tc.want {
				t.Errorf("authorizedDomainsLockout(%q, %v, %v) = %v, want %v", tc.email, tc.domains, tc.admins, got, tc.want)
			}
		})
	}
}

// Test identity clamp: for an admin-kind test identity admin_emails grants
// no bypass in the lockout rule, so a test admin never counts as a real
// admin. With the test admin tier on and no store, the kind lookup fails
// and the caller is treated as admin-kind (fail closed).
func TestRejectAuthorizedDomainsLockout_TestIdentityClamp(t *testing.T) {
	const email = "admin@example.com"
	ctx := contextWithIdentity(context.Background(), NewAuthenticatedUser("u-ta", email, "Test Admin", "admin", "api"))
	for _, tc := range []struct {
		name    string
		tierOn  bool
		domains []string
		refused bool
	}{
		{"tier off: admin_emails allows", false, []string{"other.example"}, false},
		{"tier on, test identity: admin_emails does not allow", true, []string{"other.example"}, true},
		{"tier on, test identity: a matching domain still allows", true, []string{"example.com"}, false},
		{"tier on, test identity: an empty list still allows", true, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{}
			if tc.tierOn {
				srv.testAdminGate = testAdminGate{hubAdmin: true}
			}
			rr := httptest.NewRecorder()
			got := srv.rejectAuthorizedDomainsLockout(ctx, rr, email, tc.domains, []string{email})
			if got != tc.refused {
				t.Fatalf("refused = %v, want %v (%d %s)", got, tc.refused, rr.Code, rr.Body.String())
			}
			if tc.refused {
				assertLockoutRefusal(t, rr, tc.domains...)
			}
		})
	}
}

// assertLockoutRefusal checks a 422 in the settings error envelope, naming
// the field and no domain or email.
func assertLockoutRefusal(t *testing.T, rr *httptest.ResponseRecorder, hidden ...string) {
	t.Helper()
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	if body.Error.Code != ErrCodeValidationError || body.Error.Details["field"] != authorizedDomainsKey ||
		body.Error.Message != authorizedDomainsLockoutMessage {
		t.Errorf("refusal = %+v", body.Error)
	}
	for _, h := range append(hidden, "admin@example.com") {
		if strings.Contains(rr.Body.String(), h) {
			t.Errorf("refusal echoes %q: %s", h, rr.Body.String())
		}
	}
}

func accessRowAbsent(t *testing.T, fakeStore *fakeHubSettingStore) bool {
	t.Helper()
	_, err := fakeStore.GetHubSetting(context.Background(), "access")
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("reading access row: %v", err)
	}
	return errors.Is(err, store.ErrNotFound)
}

// The saving admin is admin@example.com (adminRequest).
func TestPutServerConfigDB_AuthorizedDomainsLockout(t *testing.T) {
	t.Run("list without the admin's domain is refused, nothing written", func(t *testing.T) {
		srv, fakeStore, ops := newDomainsDBServer(t, nil, nil, nil)
		rr := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"authorized_domains":["other.example"]}}}`)
		assertLockoutRefusal(t, rr, "other.example")
		if !accessRowAbsent(t, fakeStore) {
			t.Error("access row written on refusal")
		}
	})
	t.Run("refused save keeps the stored row", func(t *testing.T) {
		srv, fakeStore, ops := newDomainsDBServer(t, nil, nil, nil)
		saveAccess(t, ops, `{"authorized_domains":["example.com"]}`)
		before, _ := fakeStore.GetHubSetting(context.Background(), "access")
		rr := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"authorized_domains":["other.example"]}}}`)
		assertLockoutRefusal(t, rr, "other.example")
		after, _ := fakeStore.GetHubSetting(context.Background(), "access")
		if after.Revision != before.Revision || string(after.Value) != string(before.Value) {
			t.Errorf("access row changed on refusal: %s rev %d -> %s rev %d", before.Value, before.Revision, after.Value, after.Revision)
		}
	})
	for _, tc := range []struct {
		name, seed, body string
	}{
		{"list with the admin's domain", "", `{"server":{"auth":{"authorized_domains":["other.example","example.com"]}}}`},
		{"wildcard list matching the admin", "", `{"server":{"auth":{"authorized_domains":["*.com"]}}}`},
		{"admin_emails in the same body", "", `{"server":{"hub":{"admin_emails":["admin@example.com"]},"auth":{"authorized_domains":["other.example"]}}}`},
		{"admin_emails kept from the row", `{"admin_emails":["Admin@Example.com"]}`, `{"server":{"auth":{"authorized_domains":["other.example"]}}}`},
		{"cleared list with no startup list", `{"authorized_domains":["other.example"],"admin_emails":["admin@example.com"]}`, `{"server":{"auth":{"authorized_domains":[]}}}`},
		{"unrelated access key", `{"authorized_domains":["other.example"]}`, `{"server":{"auth":{"user_access_mode":"open"}}}`},
	} {
		t.Run("allowed: "+tc.name, func(t *testing.T) {
			srv, _, ops := newDomainsDBServer(t, nil, nil, nil)
			if tc.seed != "" {
				saveAccess(t, ops, tc.seed)
				applyOps(srv, ops)
			}
			if rr := putServerConfigDB(t, srv, ops, tc.body); rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
			}
		})
	}
	t.Run("removing the admin from admin_emails is refused", func(t *testing.T) {
		srv, _, ops := newDomainsDBServer(t, nil, nil, nil)
		saveAccess(t, ops, `{"admin_emails":["admin@example.com"],"authorized_domains":["other.example"]}`)
		applyOps(srv, ops)
		rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"admin_emails":["root@other.example"]}}}`)
		assertLockoutRefusal(t, rr, "other.example")
	})
	t.Run("clearing to a startup list without the admin is refused", func(t *testing.T) {
		srv, _, ops := newDomainsDBServer(t, []string{"startup.example"}, nil, nil)
		saveAccess(t, ops, `{"authorized_domains":["example.com"]}`)
		applyOps(srv, ops)
		rr := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"authorized_domains":[]}}}`)
		assertLockoutRefusal(t, rr, "startup.example")
	})
	// On a pinning node the saved list is still checked: it is stored in
	// the shared database, applies on unpinned replicas, and applies here
	// once the pin is removed (scr3 R-1).
	t.Run("pinned node still checks the saved list", func(t *testing.T) {
		bootK, envK := pinnedDomainsKoanfs(t, "env.example")
		srv, fakeStore, ops := newDomainsDBServer(t, []string{"env.example"}, bootK, envK)
		rr := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"authorized_domains":["other.example"]}}}`)
		assertLockoutRefusal(t, rr, "other.example", "env.example")
		if !accessRowAbsent(t, fakeStore) {
			t.Error("access row written on refusal")
		}
	})
	t.Run("pinned node: an allowed saved list is stored and reported skipped", func(t *testing.T) {
		bootK, envK := pinnedDomainsKoanfs(t, "env.example")
		srv, fakeStore, ops := newDomainsDBServer(t, []string{"env.example"}, bootK, envK)
		rr := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"authorized_domains":["example.com"]}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), authorizedDomainsPinnedReason) {
			t.Errorf("response does not report the pinned key skipped: %s", rr.Body.String())
		}
		if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"env.example"}) {
			t.Errorf("running = %v, want the pinned [env.example]", got)
		}
		access, _ := readAccessRow(t, fakeStore)
		if !slices.Equal(access.AuthorizedDomains, []string{"example.com"}) {
			t.Errorf("stored authorized_domains = %v, want [example.com]", access.AuthorizedDomains)
		}
	})
	t.Run("pinned list still checks admin_emails", func(t *testing.T) {
		bootK, envK := pinnedDomainsKoanfs(t, "env.example")
		srv, _, ops := newDomainsDBServer(t, []string{"env.example"}, bootK, envK)
		saveAccess(t, ops, `{"admin_emails":["admin@example.com"]}`)
		applyOps(srv, ops)
		rr := putServerConfigDB(t, srv, ops, `{"server":{"hub":{"admin_emails":["root@env.example"]}}}`)
		assertLockoutRefusal(t, rr, "env.example")
	})
	t.Run("principal without an email is not checked", func(t *testing.T) {
		srv, _, ops := newDomainsDBServer(t, nil, nil, nil)
		r := httptest.NewRequest(http.MethodPut, "/api/v1/admin/server-config",
			strings.NewReader(`{"server":{"auth":{"authorized_domains":["other.example"]}}}`))
		r.Header.Set("Content-Type", "application/json")
		u := NewAuthenticatedUser("u-noemail", "", "No Email", "admin", "cli")
		r = r.WithContext(contextWithCredentialContext(contextWithIdentity(r.Context(), u), CredentialContext{Kind: CredentialKindInteractive}))
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, r, ops)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
		}
	})
}

// GET shows the running (startup) list while none is saved, and sending it
// back is an echo: nothing is stored and the lockout check does not run.
func TestServerConfigDB_AuthorizedDomainsUnsetShowsRunningValue(t *testing.T) {
	srv, fakeStore, ops := newDomainsDBServer(t, []string{"startup.example"}, nil, nil)
	applyOps(srv, ops)

	resp := getServerConfigDB(t, srv, ops)
	if resp.Server == nil || resp.Server.Auth == nil || !slices.Equal(resp.Server.Auth.AuthorizedDomains, []string{"startup.example"}) {
		t.Fatalf("GET authorized_domains = %+v, want the running [startup.example]", resp.Server)
	}

	// admin@example.com is outside startup.example: a stored value would
	// be refused, so a 200 shows the echo was recognised.
	rr := putServerConfigDB(t, srv, ops, `{"server":{"auth":{"authorized_domains":["startup.example"]}}}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("echo PUT status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if !accessRowAbsent(t, fakeStore) {
		access, _ := readAccessRow(t, fakeStore)
		if len(access.AuthorizedDomains) != 0 {
			t.Errorf("echo stored authorized_domains = %v", access.AuthorizedDomains)
		}
	}
}

// With no pin, a saved list that differs from settings.yaml is reported as
// superseding it; a row without a list supersedes nothing (the startup
// value applies).
func TestServerConfigDB_AuthorizedDomainsSuperseded(t *testing.T) {
	bootK := newFileKoanf(t, map[string]any{authorizedDomainsKey: []any{"file.example"}})
	superseded := func(srv *Server, ops *OperationalSettings) bool {
		for _, sk := range getServerConfigDB(t, srv, ops).SupersededKeys["access"] {
			if sk.Key == authorizedDomainsKey {
				return true
			}
		}
		return false
	}

	srv, fakeStore, ops := newDomainsDBServer(t, []string{"file.example"}, bootK, nil)
	fakeStore.seedWithOrigin("access", json.RawMessage(`{"authorized_domains":["saved.example"]}`), "managed")
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !superseded(srv, ops) {
		t.Error("a saved list differing from settings.yaml is not reported superseded")
	}

	srv, fakeStore, ops = newDomainsDBServer(t, []string{"file.example"}, bootK, nil)
	fakeStore.seedWithOrigin("access", json.RawMessage(`{"admin_emails":["a@file.example"]}`), "managed")
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if superseded(srv, ops) {
		t.Error("a row without a list is reported superseding the settings.yaml list")
	}
}

// newDomainsFileServer returns a file-mode hub (no OperationalSettings)
// with a settings.yaml under a temporary HOME.
func newDomainsFileServer(t *testing.T, startup []string) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, name := range []string{"SCION_SERVER_AUTH_AUTHORIZEDDOMAINS", "SCION_SERVER_HUB_ADMINEMAILS"} {
		t.Setenv(name, "")
	}
	dir := filepath.Join(home, ".scion")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.yaml")
	if err := os.WriteFile(path, []byte("schema_version: \"1\"\nserver:\n  hub:\n    port: 9810\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := &Server{dbDriver: "sqlite", maintenance: NewMaintenanceState(false, "")}
	srv.config.AuthorizedDomains = startup
	srv.recordStartupLayer1Values()
	return srv, path
}

func putAuthorizedDomainsServerConfigFile(srv *Server, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.handlePutServerConfig(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body))
	return rr
}

func TestPutServerConfigFile_AuthorizedDomains(t *testing.T) {
	t.Run("refused, settings.yaml untouched", func(t *testing.T) {
		srv, path := newDomainsFileServer(t, nil)
		before, _ := os.ReadFile(path)
		rr := putAuthorizedDomainsServerConfigFile(srv, `{"server":{"auth":{"authorized_domains":["other.example"]}}}`)
		assertLockoutRefusal(t, rr, "other.example")
		if after, _ := os.ReadFile(path); string(after) != string(before) {
			t.Errorf("settings.yaml changed on refusal:\n%s", after)
		}
	})
	t.Run("allowed list applies without a restart", func(t *testing.T) {
		srv, _ := newDomainsFileServer(t, nil)
		rr := putAuthorizedDomainsServerConfigFile(srv, `{"server":{"auth":{"authorized_domains":["example.com"]}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"example.com"}) {
			t.Errorf("running = %v, want [example.com]", got)
		}
	})
	t.Run("admin_emails in the same body allows", func(t *testing.T) {
		srv, _ := newDomainsFileServer(t, nil)
		rr := putAuthorizedDomainsServerConfigFile(srv, `{"server":{"hub":{"admin_emails":["admin@example.com"]},"auth":{"authorized_domains":["other.example"]}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("env pin wins and is not checked", func(t *testing.T) {
		srv, _ := newDomainsFileServer(t, []string{"env.example"})
		t.Setenv("SCION_SERVER_AUTH_AUTHORIZEDDOMAINS", "env.example")
		rr := putAuthorizedDomainsServerConfigFile(srv, `{"server":{"auth":{"authorized_domains":["other.example"]}}}`)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
		}
		if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"env.example"}) {
			t.Errorf("running = %v, want the pinned [env.example]", got)
		}
	})
}

func resetAccessSection(srv *Server) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.handleAdminServerConfigSectionReset(rr, adminRequest(http.MethodDelete, "/api/v1/admin/server-config/sections/access", ""))
	return rr
}

// A reset of the access section returns it to the bootstrap values; like a
// save, it may not leave access settings that refuse sign-in for the
// account making it (scr3 F-1). The saving admin is admin@example.com.
func TestResetAccessSection_AuthorizedDomainsLockout(t *testing.T) {
	t.Run("refused when the startup list excludes the admin; row kept", func(t *testing.T) {
		srv, fakeStore, ops := newDomainsDBServer(t, []string{"startup.example"}, nil, nil)
		saveAccess(t, ops, `{"authorized_domains":["example.com"]}`)
		applyOps(srv, ops)
		rr := resetAccessSection(srv)
		assertLockoutRefusal(t, rr, "startup.example", "example.com")
		if accessRowAbsent(t, fakeStore) {
			t.Fatal("access row deleted on refusal")
		}
		if got := srv.AuthorizedDomains(); !slices.Equal(got, []string{"example.com"}) {
			t.Errorf("running = %v, want the saved [example.com]", got)
		}
	})
	for _, tc := range []struct {
		name string
		boot map[string]any
	}{
		{"bootstrap list includes the admin's domain", map[string]any{authorizedDomainsKey: []any{"example.com"}}},
		{"admin in bootstrap admin_emails", map[string]any{"server.hub.admin_emails": []any{"admin@example.com"}}},
	} {
		t.Run("allowed: "+tc.name, func(t *testing.T) {
			srv, fakeStore, ops := newDomainsDBServer(t, []string{"startup.example"}, newFileKoanf(t, tc.boot), nil)
			saveAccess(t, ops, `{"authorized_domains":["example.com"]}`)
			applyOps(srv, ops)
			rr := resetAccessSection(srv)
			if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want success: %s", rr.Code, rr.Body.String())
			}
			if !accessRowAbsent(t, fakeStore) {
				t.Error("access row not deleted")
			}
		})
	}
	t.Run("allowed: no domain list anywhere", func(t *testing.T) {
		srv, fakeStore, ops := newDomainsDBServer(t, nil, nil, nil)
		saveAccess(t, ops, `{"authorized_domains":["example.com"]}`)
		applyOps(srv, ops)
		if rr := resetAccessSection(srv); rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want success: %s", rr.Code, rr.Body.String())
		}
		if !accessRowAbsent(t, fakeStore) {
			t.Error("access row not deleted")
		}
	})
	t.Run("pinned node: refused when the pinned list excludes the admin", func(t *testing.T) {
		bootK, envK := pinnedDomainsKoanfs(t, "env.example")
		srv, fakeStore, ops := newDomainsDBServer(t, []string{"env.example"}, bootK, envK)
		saveAccess(t, ops, `{"admin_emails":["admin@example.com"]}`)
		applyOps(srv, ops)
		// Running admin_emails holds the admin, but the reset's lists come
		// from the bootstrap section; with no bootstrap admin_emails the
		// running list is kept, so the admin stays allowed: a control.
		if rr := resetAccessSection(srv); rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
			t.Fatalf("control: status = %d, want success: %s", rr.Code, rr.Body.String())
		}

		srv, fakeStore, ops = newDomainsDBServer(t, []string{"env.example"}, bootK, envK)
		saveAccess(t, ops, `{"authorized_domains":["example.com"]}`)
		applyOps(srv, ops)
		rr := resetAccessSection(srv)
		assertLockoutRefusal(t, rr, "env.example", "example.com")
		if accessRowAbsent(t, fakeStore) {
			t.Error("access row deleted on refusal")
		}
	})
}

// A nil OperationalSettings refuses the access reset (fail closed) rather
// than skipping the check. The reset handler already refuses such a hub
// before the check (handleAdminServerConfigSectionReset), so this pins the
// helper on its own.
func TestRejectAccessResetLockout_NilOpsRefuses(t *testing.T) {
	srv := &Server{}
	ctx := contextWithIdentity(context.Background(), NewAuthenticatedUser("u1", "admin@example.com", "Admin", "admin", "cli"))
	rr := httptest.NewRecorder()
	if !srv.rejectAccessResetLockout(ctx, rr, nil) {
		t.Fatal("rejectAccessResetLockout(nil ops) = false, want a refusal")
	}
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
}
