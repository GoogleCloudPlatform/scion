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

package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
)

// Tests for the git rule of relinkSharedProjectIdentity (ptone/scion#4476):
// a git project is carried over to the current ID only when the Hub names
// the recorded ID as an earlier ID of the project.

// toGitProject turns the fixture's workspace into a git project: .scion is a
// directory whose project-id file names relinkOldID. workspacePath is
// recorded in the earlier config's settings.yaml unless it is empty.
func (f *relinkFixture) toGitProject(t *testing.T, workspacePath string) {
	t.Helper()
	if err := os.Remove(f.dotScion); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(f.dotScion, relinkOldID); err != nil {
		t.Fatal(err)
	}
	writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), workspacePath)
}

func (f *relinkFixture) relinkWith(slug string, previousIDs []string) error {
	return f.srv.relinkSharedProjectIdentity(context.Background(), f.root, slug, relinkNewID, relinkAgent, previousIDs, false)
}

// relinkInScope runs the re-link with unrecordedAllowed set, as for a
// workstation-mode broker serving its co-located hub, and the dispatch slug
// "proj" (the fixture's slug).
func (f *relinkFixture) relinkInScope(previousIDs []string) error {
	return f.srv.relinkSharedProjectIdentity(context.Background(), f.root, "proj", relinkNewID, relinkAgent, previousIDs, true)
}

func (f *relinkFixture) gitProjectID(t *testing.T) string {
	t.Helper()
	id, err := config.ReadProjectID(f.dotScion)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// assertGitUnchanged checks that nothing moved and the project-id file
// still names relinkOldID.
func (f *relinkFixture) assertGitUnchanged(t *testing.T) {
	t.Helper()
	if got := f.gitProjectID(t); got != relinkOldID {
		t.Fatalf("project-id rewritten to %q", got)
	}
	assertState(t, f.oldRoot)
	assertAbsent(t, f.newRoot)
}

// startWith sends a shared-workspace start carrying previousIDs.
func (f *relinkFixture) startWith(t *testing.T, previousIDs string) *httptest.ResponseRecorder {
	t.Helper()
	return f.startWithSlug(t, previousIDs, "")
}

// startWithSlug is startWith with the dispatch's projectSlug ("" omits it).
func (f *relinkFixture) startWithSlug(t *testing.T, previousIDs, slug string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"sharedWorkspace": true, "projectPath": ` + strconvQuote(f.root)
	if slug != "" {
		body += `, "projectSlug": ` + strconvQuote(slug)
	}
	if previousIDs != "" {
		body += `, "previousProjectIds": ` + previousIDs
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/start?projectId="+relinkNewID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	return w
}

func assertIdentityConflict(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "verifies the project identity") {
		t.Fatalf("expected 409 identity mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

// TestRelinkGit_ListedEarlierIDMoves: a git project whose project-id names an
// ID the Hub lists is carried over: the config directory moves with the
// agent's state and the project-id file is rewritten, when the earlier
// config records this workspace.
func TestRelinkGit_ListedEarlierIDMoves(t *testing.T) {
	for _, tc := range []struct {
		name string
		copy bool
	}{
		{name: "workspace_path names this workspace"},
		{name: "verified copy", copy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelinkFixture(t)
			f.toGitProject(t, f.root)
			if tc.copy {
				forceCopyPath(t)
			}
			if err := f.relinkWith("", []string{relinkOldID}); err != nil {
				t.Fatal(err)
			}
			if got := f.gitProjectID(t); got != relinkNewID {
				t.Fatalf("project-id = %q, want %q", got, relinkNewID)
			}
			assertState(t, f.newRoot)
			assertAbsent(t, f.oldRoot)
			if err := verifySharedProjectIdentity(f.root, relinkNewID); err != nil {
				t.Fatalf("identity check after re-link: %v", err)
			}
		})
	}
}

// TestRelinkGit_StartWithListResumes: the start body's previousProjectIds
// reaches the re-link, and the start is not refused.
func TestRelinkGit_StartWithListResumes(t *testing.T) {
	f := newRelinkFixture(t)
	f.toGitProject(t, f.root)
	w := f.startWith(t, `["`+relinkOldID+`"]`)
	if w.Code == http.StatusConflict {
		t.Fatalf("start refused: %s", w.Body.String())
	}
	if got := f.gitProjectID(t); got != relinkNewID {
		t.Fatalf("project-id = %q, want %q", got, relinkNewID)
	}
	assertState(t, f.newRoot)
	assertAbsent(t, f.oldRoot)
}

// TestRelinkGit_RestartWithListResumes: the restart body's
// previousProjectIds reaches the re-link.
func TestRelinkGit_RestartWithListResumes(t *testing.T) {
	f := newRelinkFixture(t)
	f.toGitProject(t, f.root)
	body := `{"sharedWorkspace": true, "projectPath": ` + strconvQuote(f.root) + `, "previousProjectIds": ["` + relinkOldID + `"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/restart?projectId="+relinkNewID, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(w, req)
	if w.Code == http.StatusConflict {
		t.Fatalf("restart refused: %s", w.Body.String())
	}
	if got := f.gitProjectID(t); got != relinkNewID {
		t.Fatalf("project-id = %q, want %q", got, relinkNewID)
	}
	assertState(t, f.newRoot)
	assertAbsent(t, f.oldRoot)
}

// TestRelinkGit_NotListedStillConflicts: without a matching list entry a git
// project is not moved and the start gets the 409, as before: no list (what
// an older Hub sends), a list naming another ID, and malformed lists, which
// are ignored as a whole.
func TestRelinkGit_NotListedStillConflicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		list string
	}{
		{name: "no list (older Hub)", list: ""},
		{name: "empty list", list: `[]`},
		{name: "unrelated ID", list: `["` + relinkOtherID + `"]`},
		{name: "list with a non-canonical entry", list: `["` + relinkOldID + `", "not-a-uuid"]`},
		{name: "list containing the current ID", list: `["` + relinkOldID + `", "` + relinkNewID + `"]`},
		{name: "upper-case entry", list: `["` + strings.ToUpper(relinkOldID) + `"]`},
		{name: "overlong list", list: `["` + relinkOldID + `", "` + relinkOtherID + `", "44444444-dddd-4ddd-8ddd-444444444444", "55555555-eeee-4eee-8eee-555555555555", "66666666-ffff-4fff-8fff-666666666666"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelinkFixture(t)
			f.toGitProject(t, f.root)
			assertIdentityConflict(t, f.startWith(t, tc.list))
			f.assertGitUnchanged(t)
		})
	}
}

// TestRelinkGit_ListedButOtherwiseRefused: a listed ID does not lift any
// other rule.
func TestRelinkGit_ListedButOtherwiseRefused(t *testing.T) {
	list := []string{relinkOldID}
	t.Run("earlier config records another workspace", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, t.TempDir())
		if err := f.relinkWith("", list); err != nil {
			t.Fatal(err)
		}
		f.assertGitUnchanged(t) // another workspace is recorded
	})
	t.Run("earlier config holds no state of this agent", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		agents := filepath.Join(f.oldRoot, config.DotScion, "agents")
		if err := os.Rename(filepath.Join(agents, relinkAgent), filepath.Join(agents, "another-agent")); err != nil {
			t.Fatal(err)
		}
		assertIdentityConflict(t, f.startWith(t, `["`+relinkOldID+`"]`))
		if got := f.gitProjectID(t); got != relinkOldID {
			t.Fatalf("project-id rewritten to %q", got)
		}
		assertAbsent(t, f.newRoot)
	})
	t.Run("dispatch slug differs from the workspace slug", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		if err := f.relinkWith("another-project", list); err != nil {
			t.Fatal(err)
		}
		f.assertGitUnchanged(t)
	})
	t.Run("symlinked project-id", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		idPath := filepath.Join(f.dotScion, projectkeys.ProjectIDFile)
		target := filepath.Join(t.TempDir(), "project-id")
		writeRelinkFile(t, target, relinkOldID+"\n")
		if err := os.Remove(idPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, idPath); err != nil {
			t.Fatal(err)
		}
		if err := f.relinkWith("", list); err != nil {
			t.Fatal(err)
		}
		if link, err := os.Readlink(idPath); err != nil || link != target {
			t.Fatalf("project-id symlink changed: %q, %v", link, err)
		}
		assertState(t, f.oldRoot)
		assertAbsent(t, f.newRoot)
	})
	t.Run("target directory exists", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		writeRelinkFile(t, filepath.Join(f.newRoot, "x"), "x")
		if err := f.relinkWith("", list); err == nil {
			t.Fatal("expected a refusal when the target exists")
		}
		if got := f.gitProjectID(t); got != relinkOldID {
			t.Fatalf("project-id rewritten to %q", got)
		}
		assertState(t, f.oldRoot)
	})
}

// assertUnrecordedRefusal checks that err is the Unrecorded refusal and that
// its message names the recovery step and carries no host path.
func assertUnrecordedRefusal(t *testing.T, err error) {
	t.Helper()
	var rerr *projectRelinkError
	if !errors.As(err, &rerr) || !rerr.Unrecorded {
		t.Fatalf("expected the Unrecorded refusal, got %v", err)
	}
	msg := err.Error()
	if !strings.Contains(msg, "records no workspace path") || !strings.Contains(msg, "nothing was changed") ||
		!strings.HasSuffix(msg, "; start the agent fresh") {
		t.Fatalf("refusal message = %q", msg)
	}
	if strings.Contains(msg, rerr.Path) || strings.Contains(msg, string(filepath.Separator)+config.DotScion) {
		t.Fatalf("refusal message carries a host path: %q", msg)
	}
}

// TestRelinkGit_UnrecordedWorkspaceRefused: a git project whose ID the Hub
// lists, but whose earlier config records no workspace_path, is refused with
// the Unrecorded error: nothing moves, the earlier directory keeps the
// agent's state and the project-id file still names the earlier ID.
func TestRelinkGit_UnrecordedWorkspaceRefused(t *testing.T) {
	list := []string{relinkOldID}
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *relinkFixture)
	}{
		{name: "settings.yaml without workspace_path", setup: func(t *testing.T, f *relinkFixture) {
			writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), "")
		}},
		{name: "empty workspace_path", setup: func(t *testing.T, f *relinkFixture) {
			writeRelinkFile(t, filepath.Join(f.oldRoot, config.DotScion, "settings.yaml"), "workspace_path: \"\"\n")
		}},
		{name: "no settings file", setup: func(t *testing.T, f *relinkFixture) {
			if err := os.Remove(filepath.Join(f.oldRoot, config.DotScion, "settings.yaml")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "copy path", setup: func(t *testing.T, f *relinkFixture) {
			writeRelinkSettings(t, filepath.Join(f.oldRoot, config.DotScion), "")
			forceCopyPath(t)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelinkFixture(t)
			f.toGitProject(t, f.root)
			tc.setup(t, f)
			assertUnrecordedRefusal(t, f.relinkWith("", list))
			f.assertGitUnchanged(t)
			if err := verifySharedProjectIdentity(f.root, relinkNewID); err == nil {
				t.Fatal("identity check passed after a refused re-link")
			}
		})
	}
	t.Run("start gets a 409 naming the recovery step", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, "")
		w := f.startWith(t, `["`+relinkOldID+`"]`)
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "records no workspace path") {
			t.Fatalf("expected 409 with the Unrecorded refusal, got %d: %s", w.Code, w.Body.String())
		}
		f.assertGitUnchanged(t)
	})
	t.Run("not listed keeps the identity conflict", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, "")
		assertIdentityConflict(t, f.startWith(t, ""))
		f.assertGitUnchanged(t)
	})
	t.Run("interrupted move, current dir records none", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, "")
		if err := os.Rename(f.oldRoot, f.newRoot); err != nil {
			t.Fatal(err)
		}
		assertUnrecordedRefusal(t, f.relinkWith("", list))
		if got := f.gitProjectID(t); got != relinkOldID {
			t.Fatalf("project-id rewritten to %q", got)
		}
		assertState(t, f.newRoot)
		assertAbsent(t, f.oldRoot)
	})
}

// TestRelinkGit_UnrecordedCarriedInScope: with unrecordedAllowed set (a
// workstation-mode broker serving its co-located hub) a listed git project
// whose earlier config records no workspace_path is carried over as before,
// including the finish of an interrupted move. A config recording another
// workspace is still not carried.
func TestRelinkGit_UnrecordedCarriedInScope(t *testing.T) {
	list := []string{relinkOldID}
	t.Run("move", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, "")
		if err := f.relinkInScope(list); err != nil {
			t.Fatal(err)
		}
		if got := f.gitProjectID(t); got != relinkNewID {
			t.Fatalf("project-id = %q, want %q", got, relinkNewID)
		}
		assertState(t, f.newRoot)
		assertAbsent(t, f.oldRoot)
	})
	t.Run("interrupted move", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, "")
		if err := os.Rename(f.oldRoot, f.newRoot); err != nil {
			t.Fatal(err)
		}
		if err := f.relinkInScope(list); err != nil {
			t.Fatal(err)
		}
		if got := f.gitProjectID(t); got != relinkNewID {
			t.Fatalf("project-id = %q, want %q", got, relinkNewID)
		}
		assertState(t, f.newRoot)
	})
	t.Run("another workspace recorded", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, t.TempDir())
		if err := f.relinkInScope(list); err != nil {
			t.Fatal(err)
		}
		f.assertGitUnchanged(t)
	})
	t.Run("not listed", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, "")
		if err := f.relinkInScope(nil); err != nil {
			t.Fatal(err)
		}
		f.assertGitUnchanged(t)
	})
	t.Run("no dispatch slug refuses", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, "")
		assertUnrecordedRefusal(t, f.srv.relinkSharedProjectIdentity(context.Background(), f.root, "", relinkNewID, relinkAgent, list, true))
		f.assertGitUnchanged(t)
	})
	t.Run("no dispatch slug, interrupted move refuses", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, "")
		if err := os.Rename(f.oldRoot, f.newRoot); err != nil {
			t.Fatal(err)
		}
		assertUnrecordedRefusal(t, f.srv.relinkSharedProjectIdentity(context.Background(), f.root, "", relinkNewID, relinkAgent, list, true))
		if got := f.gitProjectID(t); got != relinkOldID {
			t.Fatalf("project-id rewritten to %q", got)
		}
	})
	t.Run("no dispatch slug, recorded workspace still carries", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		if err := f.srv.relinkSharedProjectIdentity(context.Background(), f.root, "", relinkNewID, relinkAgent, list, true); err != nil {
			t.Fatal(err)
		}
		if got := f.gitProjectID(t); got != relinkNewID {
			t.Fatalf("project-id = %q, want %q", got, relinkNewID)
		}
	})
}

// scopeConn is a hub connection for the scope tests: co-located or not, at
// endpoint, with a hydrator so resolveHubConnection can pick it.
func scopeConn(t *testing.T, name string, colocated bool, endpoint string) *HubConnection {
	t.Helper()
	return &HubConnection{Name: name, IsColocated: colocated, HubEndpoint: endpoint, Hydrator: newTestHydrator(t)}
}

// TestRelinkGit_StartScope: through the start handler, the unrecorded carry
// happens only for a workstation-mode broker whose dispatch is bound to its
// co-located hub; every other broker refuses with the Unrecorded error and
// changes nothing.
func TestRelinkGit_StartScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		workstation bool
		conns       func(t *testing.T) []*HubConnection
		carried     bool
		noSlug      bool
	}{
		{"workstation, co-located loopback hub", true, func(t *testing.T) []*HubConnection {
			return []*HubConnection{scopeConn(t, "local", true, "http://127.0.0.1:8080")}
		}, true, false},
		{"hosted broker, co-located connection", false, func(t *testing.T) []*HubConnection {
			return []*HubConnection{scopeConn(t, "local", true, "http://127.0.0.1:8080")}
		}, false, false},
		{"workstation, remote hub dispatch", true, func(t *testing.T) []*HubConnection {
			return []*HubConnection{scopeConn(t, "remote", false, "https://hub.example.com")}
		}, false, false},
		{"workstation, co-located but non-loopback endpoint", true, func(t *testing.T) []*HubConnection {
			return []*HubConnection{scopeConn(t, "local", true, "https://hub.example.com")}
		}, false, false},
		{"workstation, two connections, no binding header", true, func(t *testing.T) []*HubConnection {
			return []*HubConnection{scopeConn(t, "local", true, "http://127.0.0.1:8080"), scopeConn(t, "remote", false, "https://hub.example.com")}
		}, false, false},
		{"workstation, no hub connection", true, func(*testing.T) []*HubConnection { return nil }, false, false},
		{"workstation, co-located hub, no dispatch slug", true, func(t *testing.T) []*HubConnection {
			return []*HubConnection{scopeConn(t, "local", true, "http://127.0.0.1:8080")}
		}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelinkFixture(t)
			f.toGitProject(t, "")
			f.srv.config.WorkstationMode = tc.workstation
			f.srv.hubMu.Lock()
			for _, c := range tc.conns(t) {
				f.srv.hubConnections[c.Name] = c
			}
			f.srv.hubMu.Unlock()
			slug := "proj"
			if tc.noSlug {
				slug = ""
			}
			w := f.startWithSlug(t, `["`+relinkOldID+`"]`, slug)
			if tc.carried {
				if w.Code == http.StatusConflict {
					t.Fatalf("start refused: %s", w.Body.String())
				}
				if got := f.gitProjectID(t); got != relinkNewID {
					t.Fatalf("project-id = %q, want %q", got, relinkNewID)
				}
				assertState(t, f.newRoot)
				assertAbsent(t, f.oldRoot)
				return
			}
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "records no workspace path") {
				t.Fatalf("expected 409 with the Unrecorded refusal, got %d: %s", w.Code, w.Body.String())
			}
			f.assertGitUnchanged(t)
		})
	}
}

// TestUnrecordedRelinkAllowed: the scope decision is true only for a
// workstation-mode broker and a request bound to its co-located loopback
// hub. A hosted broker, a flat-host broker (WorkstationMode unset), a
// missing request and a dispatch from another hub are all refused.
func TestUnrecordedRelinkAllowed(t *testing.T) {
	local := scopeConn(t, "local", true, "http://localhost:8080")
	remote := scopeConn(t, "remote", false, "https://hub.example.com")
	srv := func(workstation bool, conns ...*HubConnection) *Server {
		s := &Server{config: ServerConfig{WorkstationMode: workstation}, hubConnections: map[string]*HubConnection{}}
		for _, c := range conns {
			s.hubConnections[c.Name] = c
		}
		return s
	}
	authCtx := func(name string) context.Context {
		return context.WithValue(context.Background(), authenticatingHubConnCtxKey{}, name)
	}
	flat := &Server{config: DefaultServerConfig(), hubConnections: map[string]*HubConnection{"local": local}}
	for _, tc := range []struct {
		name string
		s    *Server
		ctx  context.Context
		r    *http.Request
		want bool
	}{
		{"workstation, co-located hub", srv(true, local), context.Background(), hostCredRequest(""), true},
		{"workstation, co-located hub named by header", srv(true, local, remote), context.Background(), hostCredRequest("local"), true},
		{"hosted broker", srv(false, local), context.Background(), hostCredRequest(""), false},
		{"flat-host broker (default config)", flat, context.Background(), hostCredRequest(""), false},
		{"no request", srv(true, local), context.Background(), nil, false},
		{"workstation, remote hub only", srv(true, remote), context.Background(), hostCredRequest(""), false},
		{"workstation, header names the remote hub", srv(true, local, remote), context.Background(), hostCredRequest("remote"), false},
		{"workstation, two hubs, no header", srv(true, local, remote), context.Background(), hostCredRequest(""), false},
		{"workstation, authenticated by another hub", srv(true, local), authCtx("remote"), hostCredRequest(""), false},
		{"workstation, no connection", srv(true), context.Background(), hostCredRequest(""), false},
		{"nil server", nil, context.Background(), hostCredRequest(""), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.s.unrecordedRelinkAllowed(tc.ctx, tc.r); got != tc.want {
				t.Errorf("unrecordedRelinkAllowed = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRelinkGit_InterruptedMoveRecovery: a git move that completed without
// its project-id rewrite is finished when the ID is listed, and not
// otherwise.
func TestRelinkGit_InterruptedMoveRecovery(t *testing.T) {
	interrupted := func(t *testing.T) *relinkFixture {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		if err := os.Rename(f.oldRoot, f.newRoot); err != nil {
			t.Fatal(err)
		}
		return f
	}
	t.Run("listed", func(t *testing.T) {
		f := interrupted(t)
		if err := f.relinkWith("", []string{relinkOldID}); err != nil {
			t.Fatal(err)
		}
		if got := f.gitProjectID(t); got != relinkNewID {
			t.Fatalf("project-id = %q, want %q", got, relinkNewID)
		}
		assertState(t, f.newRoot)
	})
	t.Run("not listed", func(t *testing.T) {
		f := interrupted(t)
		if err := f.relinkWith("", nil); err != nil {
			t.Fatal(err)
		}
		if got := f.gitProjectID(t); got != relinkOldID {
			t.Fatalf("project-id rewritten to %q", got)
		}
		assertState(t, f.newRoot)
	})
	t.Run("listed, current dir records another workspace", func(t *testing.T) {
		f := interrupted(t)
		writeRelinkSettings(t, filepath.Join(f.newRoot, config.DotScion), t.TempDir())
		if err := f.relinkWith("", []string{relinkOldID}); err != nil {
			t.Fatal(err)
		}
		if got := f.gitProjectID(t); got != relinkOldID {
			t.Fatalf("project-id rewritten to %q", got)
		}
	})
}

// TestRelinkGit_CreateOnHubNativeLayoutResumes: after a re-link the earlier
// agent row is gone with the earlier project, so a resume reaches the broker
// as a create. A create for a hub-native git workspace
// (~/.scion/projects/<slug>, resolved from the slug) with the list carries
// the state over. alignHubManagedProjectIdentity runs first on this path; it
// must leave the earlier project-id alone so the re-link still sees it.
func TestRelinkGit_CreateOnHubNativeLayoutResumes(t *testing.T) {
	clearSCIONEnv(t)
	t.Setenv("HOME", t.TempDir())
	srv, mgr := newHubDefaultsWiringServer()
	globalDir, err := config.GetGlobalDir()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(globalDir, "projects", "proj")
	dotScion := filepath.Join(root, config.DotScion)
	if err := os.MkdirAll(dotScion, 0755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(dotScion, relinkOldID); err != nil {
		t.Fatal(err)
	}
	oldRoot, ok := config.ConfinedProjectConfigRoot("proj", relinkOldID)
	newRoot, ok2 := config.ConfinedProjectConfigRoot("proj", relinkNewID)
	if !ok || !ok2 {
		t.Fatal("project config roots")
	}
	writeRelinkSettings(t, filepath.Join(oldRoot, config.DotScion), root)
	agentDir := filepath.Join(oldRoot, config.DotScion, "agents", relinkAgent)
	writeRelinkFile(t, filepath.Join(agentDir, "home", "state.txt"), relinkState)
	writeRelinkFile(t, filepath.Join(agentDir, "scion-agent.json"), `{}`)

	create := func(list string) *httptest.ResponseRecorder {
		body := `{"name": "` + relinkAgent + `", "id": "agent-uuid-relink", "slug": "` + relinkAgent + `", "provisionOnly": true,
			"projectId": "` + relinkNewID + `", "projectSlug": "proj",` + list + `
			"config": {"template": "claude", "sharedWorkspace": true}}`
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req)
		return w
	}

	// Without the list (an older Hub): 409, nothing changed.
	assertIdentityConflict(t, create(""))
	if id, err := config.ReadProjectID(dotScion); err != nil || id != relinkOldID {
		t.Fatalf("project-id = %q, %v", id, err)
	}
	assertAbsent(t, newRoot)

	w := create(` "previousProjectIds": ["` + relinkOldID + `"],`)
	if w.Code == http.StatusConflict {
		t.Fatalf("create refused: %s", w.Body.String())
	}
	if id, err := config.ReadProjectID(dotScion); err != nil || id != relinkNewID {
		t.Fatalf("project-id = %q, %v; want %q", id, err, relinkNewID)
	}
	if got, err := os.ReadFile(filepath.Join(newRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt")); err != nil || string(got) != relinkState {
		t.Fatalf("agent state at the current ID: %q, %v", got, err)
	}
	assertAbsent(t, oldRoot)
	if !mgr.provisionCalled {
		t.Fatal("expected Provision to run after the re-link")
	}
}

// replaceOldSettings replaces the earlier config's settings.yaml with a
// settings file named name holding content.
func (f *relinkFixture) replaceOldSettings(t *testing.T, name, content string) {
	t.Helper()
	configDir := filepath.Join(f.oldRoot, config.DotScion)
	if err := os.Remove(filepath.Join(configDir, "settings.yaml")); err != nil {
		t.Fatal(err)
	}
	writeRelinkFile(t, filepath.Join(configDir, name), content)
}

// TestRelinkGit_SettingsFileNames: the git rule reads workspace_path from the
// settings file the config loader uses (settings.yaml, settings.yml or
// settings.json, the first present), so a config recording another
// workspace in settings.yml or settings.json is refused, one recording none
// is refused with the Unrecorded error, and anything that cannot be checked
// is refused.
func TestRelinkGit_SettingsFileNames(t *testing.T) {
	list := []string{relinkOldID}
	for _, tc := range []struct {
		name       string
		file       string
		content    func(f *relinkFixture, other string) string
		moved      bool
		unrecorded bool
	}{
		{"yml records another workspace", "settings.yml", func(_ *relinkFixture, other string) string { return "workspace_path: " + strconvQuote(other) + "\n" }, false, false},
		{"json records another workspace", "settings.json", func(_ *relinkFixture, other string) string { return `{"workspace_path": ` + strconvQuote(other) + `}` }, false, false},
		{"yml records no workspace", "settings.yml", func(*relinkFixture, string) string { return "active_profile: local\n" }, false, true},
		{"json records no workspace", "settings.json", func(*relinkFixture, string) string { return `{"active_profile": "local"}` }, false, true},
		{"yml records this workspace", "settings.yml", func(f *relinkFixture, _ string) string { return "workspace_path: " + strconvQuote(f.root) + "\n" }, true, false},
		{"json records this workspace", "settings.json", func(f *relinkFixture, _ string) string { return `{"workspace_path": ` + strconvQuote(f.root) + `}` }, true, false},
		{"unparsable json", "settings.json", func(*relinkFixture, string) string { return `{"workspace_path": ` }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelinkFixture(t)
			f.toGitProject(t, f.root)
			f.replaceOldSettings(t, tc.file, tc.content(f, t.TempDir()))
			err := f.relinkWith("", list)
			if tc.unrecorded {
				assertUnrecordedRefusal(t, err)
				f.assertGitUnchanged(t)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.moved {
				if got := f.gitProjectID(t); got != relinkNewID {
					t.Fatalf("project-id = %q, want %q", got, relinkNewID)
				}
				assertState(t, f.newRoot)
				assertAbsent(t, f.oldRoot)
				return
			}
			f.assertGitUnchanged(t)
		})
	}
	t.Run("settings.yaml wins over settings.json", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		writeRelinkFile(t, filepath.Join(f.oldRoot, config.DotScion, "settings.json"), `{"workspace_path": `+strconvQuote(t.TempDir())+`}`)
		if err := f.relinkWith("", list); err != nil {
			t.Fatal(err)
		}
		if got := f.gitProjectID(t); got != relinkNewID {
			t.Fatalf("project-id = %q, want %q", got, relinkNewID)
		}
		assertState(t, f.newRoot)
		assertAbsent(t, f.oldRoot)
	})
	t.Run("settings.yml wins over settings.json", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		f.replaceOldSettings(t, "settings.yml", "workspace_path: "+strconvQuote(f.root)+"\n")
		writeRelinkFile(t, filepath.Join(f.oldRoot, config.DotScion, "settings.json"), `{"workspace_path": `+strconvQuote(t.TempDir())+`}`)
		if err := f.relinkWith("", list); err != nil {
			t.Fatal(err)
		}
		if got := f.gitProjectID(t); got != relinkNewID {
			t.Fatalf("project-id = %q, want %q", got, relinkNewID)
		}
		assertState(t, f.newRoot)
		assertAbsent(t, f.oldRoot)
	})
	t.Run("symlinked settings.yaml", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		configDir := filepath.Join(f.oldRoot, config.DotScion)
		target := filepath.Join(t.TempDir(), "settings.yaml")
		writeRelinkFile(t, target, "active_profile: local\n")
		if err := os.Remove(filepath.Join(configDir, "settings.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(configDir, "settings.yaml")); err != nil {
			t.Fatal(err)
		}
		if err := f.relinkWith("", list); err != nil {
			t.Fatal(err)
		}
		f.assertGitUnchanged(t) // a symlinked settings file is refused
	})
	t.Run("unparsable settings.yaml", func(t *testing.T) {
		f := newRelinkFixture(t)
		f.toGitProject(t, f.root)
		writeRelinkFile(t, filepath.Join(f.oldRoot, config.DotScion, "settings.yaml"), "workspace_path: [\n")
		if err := f.relinkWith("", list); err != nil {
			t.Fatal(err)
		}
		f.assertGitUnchanged(t) // an unparsable settings file is refused
	})
}

// TestRelink_MarkerSettingsJSONRecordsWorkspace: for a marker file the
// workspace_path rule reads the same settings file, so a config whose only
// settings file is settings.json recording this workspace is carried over.
func TestRelink_MarkerSettingsJSONRecordsWorkspace(t *testing.T) {
	f := newRelinkFixture(t)
	f.replaceOldSettings(t, "settings.json", `{"workspace_path": `+strconvQuote(f.root)+`}`)
	if err := f.relink(""); err != nil {
		t.Fatal(err)
	}
	if got := f.markerID(t); got != relinkNewID {
		t.Fatalf("marker = %q, want %q", got, relinkNewID)
	}
	assertState(t, f.newRoot)
}

func TestPreviousProjectIDListed(t *testing.T) {
	for _, tc := range []struct {
		name string
		list []string
		want bool
	}{
		{"nil", nil, false},
		{"listed", []string{relinkOldID}, true},
		{"listed among others", []string{relinkOtherID, relinkOldID}, true},
		{"not listed", []string{relinkOtherID}, false},
		{"bad entry", []string{relinkOldID, "x"}, false},
		{"current ID in list", []string{relinkOldID, relinkNewID}, false},
		{"too long", []string{relinkOldID, relinkOtherID, relinkOtherID, relinkOtherID, relinkOtherID}, false},
	} {
		if got := previousProjectIDListed(tc.list, relinkOldID, relinkNewID); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRelinkGit_CreateScope: a create (how a resume after a re-link reaches
// the broker) for a hub-native git workspace whose earlier config records no
// workspace_path carries the state over only for a workstation-mode broker
// whose dispatch is bound to its co-located hub. Every other create is
// refused with the Unrecorded error and changes nothing. A create without a
// dispatch slug does not resolve the hub-native workspace at all, so nothing
// is carried over (the slug rule of relinkSharedProjectIdentity itself is
// covered by TestRelinkGit_UnrecordedCarriedInScope).
func TestRelinkGit_CreateScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		workstation bool
		colocated   bool
		slug        string
		carried     bool
		unresolved  bool
	}{
		{name: "workstation, co-located hub", workstation: true, colocated: true, slug: "proj", carried: true},
		{name: "hosted broker", workstation: false, colocated: true, slug: "proj"},
		{name: "workstation, remote hub dispatch", workstation: true, colocated: false, slug: "proj"},
		{name: "workstation, co-located hub, no dispatch slug", workstation: true, colocated: true, slug: "", unresolved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearSCIONEnv(t)
			t.Setenv("HOME", t.TempDir())
			srv, mgr := newHubDefaultsWiringServer()
			srv.config.WorkstationMode = tc.workstation
			endpoint := "http://127.0.0.1:8080"
			if !tc.colocated {
				endpoint = "https://hub.example.com"
			}
			srv.hubMu.Lock()
			srv.hubConnections["hub-1"] = scopeConn(t, "hub-1", tc.colocated, endpoint)
			srv.hubMu.Unlock()
			globalDir, err := config.GetGlobalDir()
			if err != nil {
				t.Fatal(err)
			}
			dotScion := filepath.Join(globalDir, "projects", "proj", config.DotScion)
			if err := os.MkdirAll(dotScion, 0755); err != nil {
				t.Fatal(err)
			}
			if err := config.WriteProjectID(dotScion, relinkOldID); err != nil {
				t.Fatal(err)
			}
			oldRoot, ok := config.ConfinedProjectConfigRoot("proj", relinkOldID)
			newRoot, ok2 := config.ConfinedProjectConfigRoot("proj", relinkNewID)
			if !ok || !ok2 {
				t.Fatal("project config roots")
			}
			writeRelinkSettings(t, filepath.Join(oldRoot, config.DotScion), "")
			agentDir := filepath.Join(oldRoot, config.DotScion, "agents", relinkAgent)
			writeRelinkFile(t, filepath.Join(agentDir, "home", "state.txt"), relinkState)
			writeRelinkFile(t, filepath.Join(agentDir, "scion-agent.json"), `{}`)

			slug := ""
			if tc.slug != "" {
				slug = ` "projectSlug": ` + strconvQuote(tc.slug) + `,`
			}
			body := `{"name": "` + relinkAgent + `", "id": "agent-uuid-relink", "slug": "` + relinkAgent + `", "provisionOnly": true,
				"projectId": "` + relinkNewID + `",` + slug + ` "previousProjectIds": ["` + relinkOldID + `"],
				"config": {"template": "claude", "sharedWorkspace": true}}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Scion-Hub-Connection", "hub-1")
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req)

			if tc.carried {
				if w.Code == http.StatusConflict {
					t.Fatalf("create refused: %s", w.Body.String())
				}
				if id, err := config.ReadProjectID(dotScion); err != nil || id != relinkNewID {
					t.Fatalf("project-id = %q, %v; want %q", id, err, relinkNewID)
				}
				if got, err := os.ReadFile(filepath.Join(newRoot, config.DotScion, "agents", relinkAgent, "home", "state.txt")); err != nil || string(got) != relinkState {
					t.Fatalf("agent state at the current ID: %q, %v", got, err)
				}
				assertAbsent(t, oldRoot)
				return
			}
			if !tc.unresolved {
				if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "records no workspace path") {
					t.Fatalf("expected 409 with the Unrecorded refusal, got %d: %s", w.Code, w.Body.String())
				}
				if mgr.provisionCalled {
					t.Fatal("Provision ran after a refused re-link")
				}
			}
			if id, err := config.ReadProjectID(dotScion); err != nil || id != relinkOldID {
				t.Fatalf("project-id = %q, %v; want %q", id, err, relinkOldID)
			}
			if got, err := os.ReadFile(filepath.Join(agentDir, "home", "state.txt")); err != nil || string(got) != relinkState {
				t.Fatalf("agent state at the earlier ID: %q, %v", got, err)
			}
			assertAbsent(t, newRoot)
		})
	}
}

// TestRelinkGit_RestartScope: the restart handler applies the same scope as
// start and create: a listed git project whose earlier config records no
// workspace_path is carried over for a workstation-mode broker whose
// dispatch is bound to its co-located hub, and a hosted broker refuses with
// nothing changed.
func TestRelinkGit_RestartScope(t *testing.T) {
	for _, tc := range []struct {
		name        string
		workstation bool
		carried     bool
	}{
		{name: "workstation, co-located hub", workstation: true, carried: true},
		{name: "hosted broker", workstation: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelinkFixture(t)
			f.toGitProject(t, "")
			f.srv.config.WorkstationMode = tc.workstation
			f.srv.hubMu.Lock()
			f.srv.hubConnections["local"] = scopeConn(t, "local", true, "http://127.0.0.1:8080")
			f.srv.hubMu.Unlock()
			body := `{"sharedWorkspace": true, "projectPath": ` + strconvQuote(f.root) + `, "projectSlug": "proj", "previousProjectIds": ["` + relinkOldID + `"]}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+relinkAgent+"/restart?projectId="+relinkNewID, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			f.srv.Handler().ServeHTTP(w, req)
			if tc.carried {
				if w.Code == http.StatusConflict {
					t.Fatalf("restart refused: %s", w.Body.String())
				}
				if got := f.gitProjectID(t); got != relinkNewID {
					t.Fatalf("project-id = %q, want %q", got, relinkNewID)
				}
				assertState(t, f.newRoot)
				assertAbsent(t, f.oldRoot)
				return
			}
			if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "records no workspace path") {
				t.Fatalf("expected 409 with the Unrecorded refusal, got %d: %s", w.Code, w.Body.String())
			}
			f.assertGitUnchanged(t)
		})
	}
}
