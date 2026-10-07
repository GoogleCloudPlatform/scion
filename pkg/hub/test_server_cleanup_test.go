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
	"bytes"
	"context"
	"io"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// modulePath marks goroutines running this module's code.
const modulePath = "github.com/GoogleCloudPlatform/scion/"

var (
	goroutineHeaderRE = regexp.MustCompile(`^goroutine (\d+) \[`)
	createdInRE       = regexp.MustCompile(`(?m)^created by .* in goroutine (\d+)$`)
)

type goroutineInfo struct {
	id     int64
	parent int64 // 0 when unknown (e.g. runtime-created)
	stack  string
}

// liveGoroutines parses runtime.Stack(all) into goroutines keyed by ID.
// Goroutine IDs are never reused within a process.
func liveGoroutines() map[int64]goroutineInfo {
	out := map[int64]goroutineInfo{}
	for _, block := range bytes.Split(stackDump(true), []byte("\n\n")) {
		g, ok := parseGoroutine(string(block))
		if ok {
			out[g.id] = g
		}
	}
	return out
}

func currentGoroutineID(t *testing.T) int64 {
	t.Helper()
	g, ok := parseGoroutine(string(stackDump(false)))
	if !ok {
		t.Fatal("cannot parse current goroutine ID")
	}
	return g.id
}

func stackDump(all bool) []byte {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, all)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

func parseGoroutine(block string) (goroutineInfo, bool) {
	// The header regex is anchored at the start of the block; trim so a
	// stray leading newline from the "\n\n" split cannot make a goroutine
	// silently invisible to the leak check.
	block = strings.TrimSpace(block)
	m := goroutineHeaderRE.FindStringSubmatch(block)
	if m == nil {
		return goroutineInfo{}, false
	}
	id, _ := strconv.ParseInt(m[1], 10, 64)
	g := goroutineInfo{id: id, stack: block}
	if c := createdInRE.FindStringSubmatch(block); c != nil {
		g.parent, _ = strconv.ParseInt(c[1], 10, 64)
	}
	return g, true
}

// expectedServerGoroutines are the background goroutines New() is known to
// start (and that closeTestServerBackground used to stop). The vacuity guard
// requires them to be observed, so the leak check provably exercises
// something.
var expectedServerGoroutines = map[string]int{
	"hub.(*chatLinkService).cleanupLoop(":  3, // telegram, discord, teams
	"hub.(*NonceCache).cleanup(":           1, // broker auth nonce cache
	"hub.(*PreviewService).cleanupNonces(": 1,
}

// TestTestServerCleanupStopsBackgroundGoroutines guards the removal of
// closeTestServerBackground (ptone/scion#2458): the test server helpers'
// cleanup (srv.Shutdown, which runs CleanupResources even when Start was
// never called) must stop every background goroutine the server started.
//
// Which goroutines count as "started by the server": while the server is
// live, every goroutine that did not exist before and either descends
// (via the runtime's "created by ... in goroutine N" lineage) from the
// goroutine that built the server, or runs this module's code. Lineage
// covers goroutines in other packages too (database/sql, grpc clients,
// ...), as long as their creator chain was still alive at the snapshot; a
// goroutine whose creator already exited and that runs no module code is
// not attributed. The building goroutine itself is excluded. After
// cleanup, every attributed goroutine must have exited, and no new
// goroutine running module code may remain.
//
// Known flake mode: the module-code rule (and lineage, if a creator is
// misattributed) cannot tell this server's goroutines from goroutines that
// a goroutine leaked by an EARLIER test keeps spawning (a ticker loop,
// time.AfterFunc, ...). If this test fails, check the "created by" lines in
// the failure output first: a creator that is not part of this server
// points at a leak in another test. The fix then is to restrict the
// module-code rule to goroutines whose creator chain is attributed or
// unknown.
func TestTestServerCleanupStopsBackgroundGoroutines(t *testing.T) {
	for name, tc := range map[string]struct {
		newServer func(*testing.T) (*Server, store.Store)
		// extra lists goroutines this variant starts on top of
		// expectedServerGoroutines.
		extra map[string]int
	}{
		"testServer":               {newServer: testServer},
		"testServerWithBrokerAuth": {newServer: testServerWithBrokerAuth},
		// newTestHubServer with OIDC enabled: New() starts the OIDC key
		// cleanup and refresh loops, which used to run on
		// context.Background() and outlive Shutdown (ptone/scion#3641).
		"newTestHubServer/oidc": {
			newServer: func(t *testing.T) (*Server, store.Store) { return testOIDCServerWithRoutes(t), nil },
			extra:     map[string]int{"hub.(*OIDCKeyManager).Start": 2},
		},
	} {
		newServer := tc.newServer
		want := map[string]int{}
		for fn, n := range expectedServerGoroutines {
			want[fn] = n
		}
		for fn, n := range tc.extra {
			want[fn] += n
		}
		t.Run(name, func(t *testing.T) {
			outerID := currentGoroutineID(t)
			before := liveGoroutines()
			attributed := map[int64]goroutineInfo{}
			t.Run("server", func(t *testing.T) {
				builderID := currentGoroutineID(t)
				newServer(t)
				live := liveGoroutines()
				for changed := true; changed; {
					changed = false
					for id, g := range live {
						if _, seen := attributed[id]; seen || id == builderID {
							continue
						}
						if _, old := before[id]; old {
							continue
						}
						_, parentAttributed := attributed[g.parent]
						if g.parent == builderID || parentAttributed || strings.Contains(g.stack, modulePath) {
							attributed[id] = g
							changed = true
						}
					}
				}
				t.Logf("attributed %d goroutines to the server", len(attributed))
				for fn, want := range want {
					got := 0
					for _, g := range attributed {
						if strings.Contains(g.stack, fn) {
							got++
						}
					}
					if got < want {
						t.Errorf("vacuity guard: saw %d goroutines in %s, want >= %d; the leak check is not observing the server's goroutines", got, fn, want)
					}
				}
			})
			if t.Failed() {
				return
			}

			deadline := time.Now().Add(5 * time.Second)
			for {
				var leaked []string
				for id, g := range liveGoroutines() {
					if id == outerID {
						continue
					}
					_, wasAttributed := attributed[id]
					_, old := before[id]
					if wasAttributed || (!old && strings.Contains(g.stack, modulePath)) {
						leaked = append(leaked, g.stack)
					}
				}
				if len(leaked) == 0 {
					return
				}
				if time.Now().After(deadline) {
					sort.Strings(leaked)
					t.Fatalf("%d of %d server goroutines still running after cleanup:\n\n%s",
						len(leaked), len(attributed), strings.Join(leaked, "\n\n"))
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// leakedServerGoroutineSigs are the stack substrings of the background
// loops New() starts before its last fallible step.
var leakedServerGoroutineSigs = func() []string {
	var sigs []string
	for _, sig := range leakGuardSignatures {
		if sig != leakGuardStoreSignature {
			sigs = append(sigs, sig)
		}
	}
	return sigs
}()

// countServerLoopGoroutines counts live goroutines, not in skip, whose
// stack contains one of leakedServerGoroutineSigs.
func countServerLoopGoroutines(skip map[int64]goroutineInfo) (int, []string) {
	var stacks []string
	for id, g := range liveGoroutines() {
		if _, old := skip[id]; old {
			continue
		}
		for _, sig := range leakedServerGoroutineSigs {
			if strings.Contains(g.stack, sig) {
				stacks = append(stacks, g.stack)
				break
			}
		}
	}
	sort.Strings(stacks)
	return len(stacks), stacks
}

// TestNewFailureStopsBackgroundGoroutines: a New() that fails after it has
// started its background loops (here, at the fail-closed D4 membership
// index step, which runs after the link services, the preview engine, the
// decision audit worker and the OIDC key loops are up) must stop them
// itself, since the caller gets no *Server to shut down
// (ptone/scion#3641).
func TestNewFailureStopsBackgroundGoroutines(t *testing.T) {
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	cfg := testServerConfig()
	cfg.OIDCConfig = config.OIDCProviderConfig{Enabled: true, IssuerURL: testOIDCIssuerURL}

	before := liveGoroutines()
	// Vacuity guard: the same config on a working store does start the
	// loops, so a zero count after the failure means they were stopped.
	ok, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New on a working store: %v", err)
	}
	// Shut down even if the vacuity check below fails (Shutdown is
	// idempotent, so the explicit call after it is still fine).
	t.Cleanup(func() { _ = ok.Shutdown(context.Background()) })
	if n, _ := countServerLoopGoroutines(before); n < len(expectedServerGoroutines)+2 {
		t.Fatalf("vacuity guard: a working New started only %d background loops", n)
	}
	_ = ok.Shutdown(context.Background())

	before = liveGoroutines()
	if _, err := New(cfg, &noDBStore{Store: s}); err == nil {
		t.Fatal("New on a store without DB() succeeded; want the fail-closed D4 error")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, stacks := countServerLoopGoroutines(before)
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d background goroutines still running after New failed:\n\n%s", n, strings.Join(stacks, "\n\n"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLeakGuardReportsUnclosedStore checks that the package-exit leak guard
// (leak_guard_helpers_test.go) sees an unclosed store and names it in its
// failure output, and passes again once the store is closed.
func TestLeakGuardReportsUnclosedStore(t *testing.T) {
	// Let goroutines from earlier tests (for example the connectionOpener
	// of a store that was just closed) finish exiting before taking the
	// baseline, so one exiting between the snapshot and the check cannot
	// cancel out the new store's goroutine.
	checkPackageLeaks(io.Discard, 0, leakGuardSettle, false)
	base := scanLeakedGoroutines().total
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	var out bytes.Buffer
	if checkPackageLeaks(&out, base, 0, false) {
		t.Fatalf("leak guard passed with an open store; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "database/sql.(*DB).connectionOpener") || !strings.Contains(out.String(), "FAIL: leak guard") {
		t.Errorf("leak guard output does not list the open store's stack:\n%s", out.String())
	}
	_ = s.Close()
	out.Reset()
	if !checkPackageLeaks(&out, base, 5*time.Second, false) {
		t.Errorf("leak guard still fails after the store was closed:\n%s", out.String())
	}
}
