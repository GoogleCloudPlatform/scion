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

//go:build unix

package sshd

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/suppgroups"
)

// TestSysProcAttr pins how children are started: their own process group
// without a pty, their own session with the pty as controlling terminal
// with one, the container user's uid, gid and kept supplementary groups
// when dropping privileges, and a refusal when a drop is required but no
// container user is set.
func TestSysProcAttr(t *testing.T) {
	t.Setenv(suppgroups.EnvVar, "2000,3000")
	defer suppgroups.SetGetgroupsForTest(func() ([]int, error) { return []int{2000, 4000}, nil })()

	a, err := sysProcAttr(User{UID: 1000, GID: 1001}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Setpgid || a.Setsid || a.Setctty {
		t.Fatalf("no pty: Setpgid=%v Setsid=%v Setctty=%v, want own process group only", a.Setpgid, a.Setsid, a.Setctty)
	}
	if a.Credential == nil || a.Credential.Uid != 1000 || a.Credential.Gid != 1001 || !slices.Equal(a.Credential.Groups, []uint32{2000}) {
		t.Fatalf("credential = %+v, want uid 1000, gid 1001, groups [2000]", a.Credential)
	}

	a, err = sysProcAttr(User{UID: 1000, GID: 1001}, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.Setpgid || !a.Setsid || !a.Setctty {
		t.Fatalf("pty: Setpgid=%v Setsid=%v Setctty=%v, want own session with controlling tty", a.Setpgid, a.Setsid, a.Setctty)
	}
	if a.Credential == nil || a.Credential.Uid != 1000 {
		t.Fatalf("pty credential = %+v", a.Credential)
	}

	a, err = sysProcAttr(User{}, false)
	if err != nil || a.Credential != nil {
		t.Fatalf("no container user: %+v, %v; want this process's identity", a, err)
	}

	for _, u := range []User{{RequirePrivilegeDrop: true}, {UID: 1000, RequirePrivilegeDrop: true}, {GID: 1000, RequirePrivilegeDrop: true}} {
		if _, err := sysProcAttr(u, false); !errors.Is(err, errPrivilegeDrop) {
			t.Fatalf("%+v: err = %v, want errPrivilegeDrop", u, err)
		}
	}
}

// TestPrivilegeDropRequiredRefusesSessions: with a required drop and no
// container user, shell and exec are refused.
func TestPrivilegeDropRequiredRefusesSessions(t *testing.T) {
	cfg := testConfig(t)
	cfg.User = User{RequirePrivilegeDrop: true}
	client := serve(t, cfg, "scion")
	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Run("true"); err == nil {
		t.Fatal("exec accepted")
	}
	sess, err = client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Shell(); err == nil {
		t.Fatal("shell accepted")
	}
	_ = sess.Close()
}

// TestLoginShellFromPasswd: the login shell is the user's passwd shell
// when it is an executable file, else the fallback.
func TestLoginShellFromPasswd(t *testing.T) {
	dir := t.TempDir()
	passwd := filepath.Join(dir, "passwd")
	data := "root:x:0:0:root:/root:/bin/bash\n" +
		"agent:x:4242:4242::/home/agent:/bin/sh\n" +
		"gone:x:4343:4343::/home/gone:/nonexistent/shell\n" +
		"rel:x:4444:4444::/home/rel:sh\n"
	if err := os.WriteFile(passwd, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	fallback := "/bin/sh"
	if isExecutable("/bin/bash") {
		fallback = "/bin/bash"
	}
	tests := []struct {
		uid  int
		want string
	}{
		{4242, "/bin/sh"},
		{4343, fallback},
		{4444, fallback},
		{5555, fallback},
	}
	for _, tt := range tests {
		if got := loginShell(passwd, User{UID: tt.uid, GID: tt.uid}); got != tt.want {
			t.Errorf("uid %d: shell %q, want %q", tt.uid, got, tt.want)
		}
	}
	if got := loginShell(filepath.Join(dir, "missing"), User{UID: 4242, GID: 4242}); got != fallback {
		t.Errorf("no passwd file: shell %q, want %q", got, fallback)
	}
}

// TestLoginShellRootlessUsesContainerUser: in rootless mode (container
// user named, uid not positive) the shell is the container user's passwd
// shell, not the shell of this process's uid.
func TestLoginShellRootlessUsesContainerUser(t *testing.T) {
	dir := t.TempDir()
	ownShell := filepath.Join(dir, "own-uid-shell")
	if err := os.WriteFile(ownShell, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	passwd := filepath.Join(dir, "passwd")
	data := "self:x:" + strconv.Itoa(os.Getuid()) + ":0::/:" + ownShell + "\n" +
		"scion:x:4242:4242::/home/scion:/bin/sh\n"
	if err := os.WriteFile(passwd, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loginShell(passwd, User{Username: "scion"}); got != "/bin/sh" {
		t.Fatalf("rootless shell %q, want the container user's /bin/sh", got)
	}
	// Without a container user name, this process's uid decides.
	if got := loginShell(passwd, User{}); got != ownShell {
		t.Fatalf("shell without a container user %q, want %q", got, ownShell)
	}
}
