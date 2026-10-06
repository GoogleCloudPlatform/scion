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
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Store fault injection without reassigning Server.store mid-test.
//
// The race this prevents: many tests inject a store failure by writing
// srv.store = &someFailingStore{Store: s} after their setup. But setup that
// goes through the Server (seedProjectCreatorMembership ->
// createProjectOwnerRoleBinding, HTTP agent creates, group/schedule/settings
// mutations, ...) calls emitMutationAudit, which starts a fire-and-forget
// goroutine that reads s.store. The later plain field write races that read
// (ptone/scion#2099, ptone/scion#2577, follow-up ptone/scion#3184).
//
// The fix is to install the failing wrapper once, right after the Server is
// constructed and before any audited setup, and to keep it transparent
// until the test needs the fault:
//
//	srv, s := testServer(t)
//	failing, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *myFailingStore {
//		return &myFailingStore{Store: inner, fault: f}
//	})
//	// ... audited setup, through srv or the raw store s ...
//	fault.Arm() // where the test used to assign srv.store
//
// and in the wrapper, delegate while the switch is not active:
//
//	func (w *myFailingStore) GetThing(ctx context.Context, id string) (*store.Thing, error) {
//		if !w.fault.Active() {
//			return w.Store.GetThing(ctx, id)
//		}
//		return nil, errInjected
//	}
//
// testServerWithStoreFault bundles testServer and installStoreFault. Fixtures
// that build their own server can call installStoreFault themselves, as long
// as they do it before their first audited step. The wrapper is installed
// on srv.store only; it is deliberately not passed to New(), because New()
// relies on optional interfaces of the raw store (e.g. DB() for the
// membership index) and the services it builds keep the raw store, which
// matches what the old post-setup reassignment did.
//
// Caveat: "transparent until armed" covers store.Store methods only. A
// wrapper that embeds store.Store hides the raw store's OPTIONAL interfaces
// (DB() *sql.DB, Dialect() string, ...) from the moment it is installed,
// armed or not, so Server code that type-asserts srv.store takes its
// fallback path for the rest of the test, setup included. If the code under
// test type-asserts srv.store, forward those methods in the wrapper. Server
// call sites that type-assert srv.store:
//   - handleHealthSummary (handlers_health_summary.go): DB(), pool stats
//   - storeDB (utc_timestamp_normalize.go): DB() + Dialect(); used by the
//     UTC timestamp normalization in the same file and by
//     admin_maintenance.go
//   - StartBackgroundServices (server.go): DB(), only if the test starts
//     background services after installing the wrapper
//
// (runMembershipMigration also asserts DB(), but it runs inside New(),
// before the wrapper exists.)

// storeFaultSwitch gates a store wrapper's failure injection. Wrappers
// delegate to the inner store until Arm is called. A nil *storeFaultSwitch
// is always active, so a wrapper built without a switch keeps failing
// unconditionally (the pre-switch behavior).
type storeFaultSwitch struct {
	armed atomic.Bool
}

// Arm turns on failure injection for every wrapper sharing this switch. It
// is safe to call from any goroutine.
func (f *storeFaultSwitch) Arm() { f.armed.Store(true) }

// Active reports whether a wrapper should inject its failure now.
func (f *storeFaultSwitch) Active() bool { return f == nil || f.armed.Load() }

// installStoreFault wraps srv's current store with wrap, installs the
// wrapper as srv.store, and returns it together with its (disarmed) switch.
// Call it right after constructing srv and before any setup that can reach
// emitMutationAudit or otherwise start goroutines reading srv.store; never
// call it on a server whose setup has already run.
func installStoreFault[W store.Store](t testing.TB, srv *Server, wrap func(inner store.Store, fault *storeFaultSwitch) W) (W, *storeFaultSwitch) {
	t.Helper()
	fault := &storeFaultSwitch{}
	wrapped := wrap(srv.store, fault)
	srv.store = wrapped
	return wrapped, fault
}

// testServerWithStoreFault is testServer followed immediately by
// installStoreFault. It returns the unwrapped store, so setup that writes
// through it bypasses the wrapper, plus the wrapper and its switch.
func testServerWithStoreFault[W store.Store](t *testing.T, wrap func(inner store.Store, fault *storeFaultSwitch) W) (*Server, store.Store, W, *storeFaultSwitch) {
	t.Helper()
	srv, s := testServer(t)
	wrapped, fault := installStoreFault(t, srv, wrap)
	return srv, s, wrapped, fault
}

// errProbeFault is returned by probeFaultStore while its switch is active.
var errProbeFault = errors.New("injected probe fault")

// probeFaultStore is a minimal switch-gated wrapper used to pin the helper
// semantics below.
type probeFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (p *probeFaultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if !p.fault.Active() {
		return p.Store.GetUser(ctx, id)
	}
	return nil, errProbeFault
}

// faultProbeInnerStore answers GetUser without a database; any other method would
// panic on the nil embedded store, which these tests never call.
type faultProbeInnerStore struct {
	store.Store
}

func (faultProbeInnerStore) GetUser(_ context.Context, id string) (*store.User, error) {
	return &store.User{ID: id}, nil
}

// TestStoreFaultSwitch pins the switch semantics that wrappers rely on,
// through the wrappers themselves. In particular a nil switch must stay
// always-active: wrappers built without a switch depend on it to keep
// failing unconditionally, e.g. the
// srv.authConfig.CredentialStore = &erroringCredentialStore{...}
// assignments in agent_credential_status_test.go
// (TestAgentAuthStoreErrorReturns503, TestAgentAuthNilCredentialNilErrorReturns503).
func TestStoreFaultSwitch(t *testing.T) {
	ctx := context.Background()
	getUser := func(w *probeFaultStore) error {
		t.Helper()
		u, err := w.GetUser(ctx, "probe-user")
		if err == nil && (u == nil || u.ID != "probe-user") {
			t.Fatalf("delegated GetUser returned %+v", u)
		}
		return err
	}

	switchless := &probeFaultStore{Store: faultProbeInnerStore{}}
	if err := getUser(switchless); !errors.Is(err, errProbeFault) {
		t.Fatalf("wrapper with nil switch: err = %v, want errProbeFault (nil switch must be always active)", err)
	}

	fault := &storeFaultSwitch{}
	a := &probeFaultStore{Store: faultProbeInnerStore{}, fault: fault}
	b := &probeFaultStore{Store: faultProbeInnerStore{}, fault: fault}
	for name, w := range map[string]*probeFaultStore{"a": a, "b": b} {
		if err := getUser(w); err != nil {
			t.Fatalf("wrapper %s before Arm: err = %v, want delegation to the inner store", name, err)
		}
	}
	fault.Arm()
	fault.Arm() // idempotent
	for name, w := range map[string]*probeFaultStore{"a": a, "b": b} {
		if err := getUser(w); !errors.Is(err, errProbeFault) {
			t.Fatalf("wrapper %s after Arm: err = %v, want errProbeFault (Arm must activate every wrapper sharing the switch)", name, err)
		}
	}

	other := &probeFaultStore{Store: faultProbeInnerStore{}, fault: &storeFaultSwitch{}}
	if err := getUser(other); err != nil {
		t.Fatalf("wrapper with a separate switch: err = %v, want delegation (switches must not be shared implicitly)", err)
	}
}

// TestInstallStoreFault pins installStoreFault / testServerWithStoreFault:
// the returned wrapper is the one installed as srv.store, it delegates until
// armed and fails afterwards, and the returned raw store bypasses it.
func TestInstallStoreFault(t *testing.T) {
	srv, s, wrapped, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *probeFaultStore {
		return &probeFaultStore{Store: inner, fault: f}
	})
	if got, ok := srv.store.(*probeFaultStore); !ok || got != wrapped {
		t.Fatalf("srv.store = %T (%p), want the returned wrapper %p", srv.store, srv.store, wrapped)
	}
	if wrapped.Store != s || wrapped.fault != fault {
		t.Fatal("wrapper must wrap the raw store returned to the caller and share the returned switch")
	}

	ctx := context.Background()
	user := &store.User{
		ID:          tid("user-store-fault-probe"),
		Email:       "store-fault-probe@example.com",
		DisplayName: "Probe",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.GetUser(ctx, user.ID); err != nil {
		t.Fatalf("disarmed wrapper must delegate: %v", err)
	}
	fault.Arm()
	if _, err := srv.store.GetUser(ctx, user.ID); !errors.Is(err, errProbeFault) {
		t.Fatalf("armed wrapper: err = %v, want errProbeFault", err)
	}
	if _, err := s.GetUser(ctx, user.ID); err != nil {
		t.Fatalf("raw store must bypass the wrapper: %v", err)
	}

	// installStoreFault on its own wraps whatever srv.store currently is.
	srv2, _ := testServer(t)
	before := srv2.store
	w2, f2 := installStoreFault(t, srv2, func(inner store.Store, f *storeFaultSwitch) *probeFaultStore {
		return &probeFaultStore{Store: inner, fault: f}
	})
	if srv2.store != store.Store(w2) || w2.Store != before || f2 == nil || f2.Active() {
		t.Fatal("installStoreFault must install a wrapper around the previous srv.store with a fresh, disarmed switch")
	}
}
