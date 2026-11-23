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

//go:build !windows

package brokercontrol

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCheck_UnixRootfulAndRootlessSockets(t *testing.T) {
	f := newFixture(t)
	xdg := filepath.Join(f.outside, "xdg-runtime")
	mkdir(t, xdg)
	setSocketTestHooks(t, f.home, map[string]string{
		"XDG_RUNTIME_DIR": xdg,
	}, 1000, 1000, nil)

	cases := []struct {
		name string
		src  string
		rule Rule
	}{
		{"rootful /var/run/docker.sock", "/var/run/docker.sock", RuleEqual},
		{"rootful /run/docker.sock", "/run/docker.sock", RuleEqual},
		{"rootful /run/podman/podman.sock", "/run/podman/podman.sock", RuleEqual},
		{"rootful /var/run/podman/podman.sock", "/var/run/podman/podman.sock", RuleEqual},
		{"rootless XDG docker.sock", filepath.Join(xdg, "docker.sock"), RuleEqual},
		{"rootless XDG podman.sock", filepath.Join(xdg, "podman", "podman.sock"), RuleEqual},
		{"rootless /run/user/<uid>/docker.sock", filepath.Join("/run/user", strconv.Itoa(1000), "docker.sock"), RuleEqual},
		{"rootless /run/user/<uid>/podman/podman.sock", filepath.Join("/run/user", strconv.Itoa(1000), "podman", "podman.sock"), RuleEqual},
		{"parent /run", "/run", RuleAncestor},
		{"parent /var/run", "/var/run", RuleAncestor},
		{"parent XDG_RUNTIME_DIR", xdg, RuleAncestor},
		{"parent XDG podman dir", filepath.Join(xdg, "podman"), RuleAncestor},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Check(tc.src, VolumeAllowed)
			if err == nil {
				t.Fatalf("Check(%q) succeeded, want refusal", tc.src)
			}
			// If the host happens to have an unreadable /run/podman, Check on
			// /run/podman/podman.sock fails closed with RuleUnresolved instead
			// of RuleEqual; either refusal is acceptable for real host paths.
			wantRefused(t, err, "")
			msg := err.Error()
			if strings.Contains(msg, ".scion") {
				t.Fatalf("refusal message for %q contains misleading .scion wording: %s", tc.src, msg)
			}
		})
	}
}

func TestCheck_SymlinkToContainerSocket(t *testing.T) {
	f := newFixture(t)
	xdg := filepath.Join(f.outside, "xdg")
	sock := filepath.Join(xdg, "docker.sock")
	write(t, sock)

	setSocketTestHooks(t, f.home, map[string]string{
		"XDG_RUNTIME_DIR": xdg,
	}, 1000, 1000, nil)

	link := filepath.Join(f.outside, "alias.sock")
	symlink(t, sock, link)

	_, err := Check(link, VolumeAllowed)
	wantSocketRefused(t, err, RuleEqual)
}

func TestCheck_SocketInUnreadableParentDoesNotBreakOtherMounts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	f := newFixture(t)
	// Simulate 0700 root:root /run/podman where a non-root broker hits EACCES
	// when resolving the socket path inside it.
	fakeRun := filepath.Join(f.outside, "fake-run")
	lockedPodman := filepath.Join(fakeRun, "podman")
	mkdir(t, lockedPodman)
	write(t, filepath.Join(lockedPodman, "podman.sock"))
	if err := os.Chmod(lockedPodman, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(lockedPodman, 0o755) })

	setSocketTestHooks(t, f.home, nil, 1000, 1000, []string{
		filepath.Join(lockedPodman, "podman.sock"),
	})

	// Resolve() must succeed despite EACCES on the socket's parent directory.
	if _, err := Resolve(); err != nil {
		t.Fatalf("Resolve() failed with EACCES socket parent: %v", err)
	}

	// Unrelated allowed mount must still succeed.
	allowed := filepath.Join(f.outside, "allowed-work")
	mkdir(t, allowed)
	if _, err := Check(allowed, VolumeAllowed); err != nil {
		t.Fatalf("unrelated allowed mount was broken by EACCES socket parent: %v", err)
	}

	// Mounting the unreadable parent directory itself is refused as an ancestor.
	_, err := Check(lockedPodman, VolumeAllowed)
	wantSocketRefused(t, err, RuleAncestor)

	// Mounting the grandparent directory is also refused as an ancestor.
	_, err = Check(fakeRun, VolumeAllowed)
	wantSocketRefused(t, err, RuleAncestor)

	// Mounting the socket inside the unreadable directory is refused (fail closed).
	_, err = Check(filepath.Join(lockedPodman, "podman.sock"), VolumeAllowed)
	wantRefused(t, err, RuleUnresolved)
}

func TestCheck_DanglingSymlinkSocketDoesNotBreakOtherMounts(t *testing.T) {
	f := newFixture(t)
	fakeRun := filepath.Join(f.outside, "fake-run")
	danglingSock := filepath.Join(fakeRun, "docker.sock")
	symlink(t, filepath.Join(f.outside, "stopped-daemon.sock"), danglingSock)

	setSocketTestHooks(t, f.home, nil, 1000, 1000, []string{danglingSock})

	if _, err := Resolve(); err != nil {
		t.Fatalf("Resolve() failed with dangling socket symlink: %v", err)
	}

	// Unrelated mount must still succeed.
	allowed := filepath.Join(f.outside, "allowed-work")
	mkdir(t, allowed)
	if _, err := Check(allowed, VolumeAllowed); err != nil {
		t.Fatalf("unrelated allowed mount was broken by dangling socket symlink: %v", err)
	}

	// Mounting the parent directory is refused as an ancestor.
	_, err := Check(fakeRun, VolumeAllowed)
	wantSocketRefused(t, err, RuleAncestor)

	// Mounting the dangling symlink itself is refused (fail closed).
	_, err = Check(danglingSock, VolumeAllowed)
	wantRefused(t, err, RuleUnresolved)
}
