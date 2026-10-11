// Copyright 2026 The Scion Authors.

package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

var (
	_ AgentHomeOwnershipRepairer = (*DockerRuntime)(nil)
	_ AgentHomeOwnershipRepairer = (*PodmanRuntime)(nil)
)

// rootfulDockerSecurityOptions is `docker info` SecurityOptions output of
// a rootful daemon without user-namespace remapping.
const rootfulDockerSecurityOptions = `["name=apparmor","name=seccomp,profile=builtin","name=cgroupns"]`

const (
	testRepairName = "scion-home-repair-0123456789abcdef"
	testRepairKey  = "fedcba9876543210"
)

// fakeRepairRuntime is a fake docker/podman binary for the repair helper.
type fakeRepairRuntime struct {
	bin, runArgs, rmCalls, psArgs string
}

// fakeRepairOpts configures newFakeRepairRuntime.
type fakeRepairOpts struct {
	info      string // `info` output
	infoFails bool
	runExit   int
	runSleep  int    // seconds; when > 0 the run sleeps instead of exiting
	psOut     string // `ps` output (leftover helper ids)
	psFails   bool
	rmFails   bool
}

// newFakeRepairRuntime writes a fake runtime binary: `info` answers
// o.info (or fails), `run` records its argv (one argument per line) and
// then sleeps or exits, `ps` records its argv and prints o.psOut, and each
// `rm` call is appended to rmCalls as one line.
func newFakeRepairRuntime(t *testing.T, o fakeRepairOpts) *fakeRepairRuntime {
	t.Helper()
	dir := t.TempDir()
	f := &fakeRepairRuntime{
		bin:     filepath.Join(dir, "fake-runtime"),
		runArgs: filepath.Join(dir, "run-args"),
		rmCalls: filepath.Join(dir, "rm-calls"),
		psArgs:  filepath.Join(dir, "ps-args"),
	}
	info := fmt.Sprintf("printf '%%s\\n' %q; exit 0", o.info)
	if o.infoFails {
		info = "echo daemon down >&2; exit 1"
	}
	// A sleeping run execs sleep, so killing the process on a timeout also
	// closes its output, as the real CLI does.
	runTail := fmt.Sprintf("exit %d", o.runExit)
	if o.runSleep > 0 {
		runTail = fmt.Sprintf("exec sleep %d", o.runSleep)
	}
	rmExit := 0
	if o.rmFails {
		rmExit = 1
	}
	psExit := 0
	if o.psFails {
		psExit = 1
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in
info) %s ;;
ps) printf '%%s\n' "$@" > %q; printf '%%b\n' %q; exit %d ;;
rm) echo "$*" >> %q; exit %d ;;
run) printf '%%s\n' "$@" > %q; echo helper-output; %s ;;
esac
exit 99
`, info, f.psArgs, o.psOut, psExit, f.rmCalls, rmExit, f.runArgs, runTail)
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func rootfulDocker() fakeRepairOpts { return fakeRepairOpts{info: rootfulDockerSecurityOptions} }

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("not recorded: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func (f *fakeRepairRuntime) ran() bool {
	_, err := os.Stat(f.runArgs)
	return err == nil
}

// rmLines returns the recorded `rm` calls, nil when there were none.
func (f *fakeRepairRuntime) rmLines(t *testing.T) []string {
	t.Helper()
	if _, err := os.Stat(f.rmCalls); err != nil {
		return nil
	}
	return readLines(t, f.rmCalls)
}

// helperName returns the --name the recorded run used.
func (f *fakeRepairRuntime) helperName(t *testing.T) string {
	t.Helper()
	args := readLines(t, f.runArgs)
	i := slices.Index(args, "--name")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("run has no --name: %q", args)
	}
	return args[i+1]
}

// goldenRepairArgs is the complete expected helper argv, written out here
// as a literal: a third source, independent of both agentHomeRepairArgs
// and the runtime allow-list, so a change to either alone fails a test.
func goldenRepairArgs(home, name, key, image, owner string, userNSHost bool) []string {
	args := []string{
		"run", "--rm", "--name", name, "--pull=never", "--network=none", "--user=0:0",
		"--cap-drop=ALL", "--cap-add=CHOWN", "--cap-add=DAC_OVERRIDE",
		"--security-opt=no-new-privileges", "--read-only", "--pids-limit=64",
		"--memory=256m", "--cpus=1",
	}
	if userNSHost {
		args = append(args, "--userns=host")
	}
	return append(args,
		"--label", "scion.helper=agent-home-ownership-"+key,
		"--volume", home+":/scion-agent-home",
		"--entrypoint", "find", image,
		"/scion-agent-home", "-xdev", "(", "-type", "d", "-o", "-type", "l", "-o", "(", "-type", "f", "-links", "1", ")", ")",
		"-exec", "chown", "-h", owner, "{}", "+",
	)
}

// newRepairHome creates an agent home outside ~/.scion that passes the
// agent-home checks, and returns its resolved path.
func newRepairHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "agents", "a1", "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := ValidateAgentHomeSource(home, "")
	if err != nil {
		t.Fatalf("fixture home refused: %v", err)
	}
	return resolved
}

func testRepairHelper(userNSHost bool) agentHomeRepairHelper {
	return agentHomeRepairHelper{
		req:        AgentHomeOwnershipRepair{HomeDir: "/srv/agents/a1/home", Image: "img:1", UID: 1002, GID: 1003},
		name:       testRepairName,
		homeKey:    testRepairKey,
		userNSHost: userNSHost,
	}
}

// The builder produces exactly the golden argv, for Docker and Podman.
func TestAgentHomeRepairArgs_Golden(t *testing.T) {
	for _, userNSHost := range []bool{false, true} {
		got, err := agentHomeRepairArgs(testRepairHelper(userNSHost))
		if err != nil {
			t.Fatal(err)
		}
		want := goldenRepairArgs("/srv/agents/a1/home", testRepairName, testRepairKey, "img:1", "1002:1003", userNSHost)
		if !slices.Equal(got, want) {
			t.Errorf("userNSHost=%v: helper args\n got %q\nwant %q", userNSHost, got, want)
		}
	}
}

// The runtime allow-list accepts exactly the golden argv, independently of
// the builder.
func TestValidateAgentHomeRepairArgs_Golden(t *testing.T) {
	for _, userNSHost := range []bool{false, true} {
		h := testRepairHelper(userNSHost)
		golden := goldenRepairArgs("/srv/agents/a1/home", testRepairName, testRepairKey, "img:1", "1002:1003", userNSHost)
		if err := validateAgentHomeRepairArgs(golden, h); err != nil {
			t.Errorf("userNSHost=%v: the allow-list refuses the golden argv: %v", userNSHost, err)
		}
		// A flag outside the allow-list, one the builder's own tests do
		// not deny explicitly, is refused.
		for _, extra := range []string{"--ipc=host", "--uts=host", "--cgroupns=host", "--security-opt=label=disable", "--ulimit=nofile=1", "--tmpfs=/x", "--group-add=0"} {
			bad := slices.Clone(golden)
			at := slices.Index(bad, "img:1")
			bad = slices.Insert(bad, at, extra)
			if err := validateAgentHomeRepairArgs(bad, h); err == nil {
				t.Errorf("userNSHost=%v: %s was not refused", userNSHost, extra)
			}
		}
	}
}

func TestAgentHomeRepairArgs(t *testing.T) {
	args, err := agentHomeRepairArgs(testRepairHelper(false))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"run", "--rm", testRepairName, "--pull=never", "--network=none", "--user=0:0",
		"--cap-drop=ALL", "--cap-add=CHOWN", "--cap-add=DAC_OVERRIDE",
		"--security-opt=no-new-privileges", "--read-only", "--pids-limit=64",
		"--memory=256m", "--cpus=1",
		"img:1", "-xdev", "1002:1003",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("helper args %q lack %q", args, want)
		}
	}
	if i := slices.Index(args, "--entrypoint"); i < 0 || args[i+1] != "find" {
		t.Errorf("helper entrypoint is not find: %q", args)
	}
	// Symlinks are changed themselves, never their targets.
	if i := slices.Index(args, "chown"); i < 0 || args[i+1] != "-h" {
		t.Errorf("chown does not use -h: %q", args)
	}
	// Only the agent home is mounted, and nothing widens the helper's
	// privileges beyond a normal start's: in particular no
	// DAC_READ_SEARCH (open_by_handle_at).
	joined := strings.Join(args, " ")
	if n := strings.Count(joined, "--volume"); n != 1 {
		t.Errorf("helper mounts %d volumes, want exactly 1", n)
	}
	if i := slices.Index(args, "--volume"); i < 0 || args[i+1] != "/srv/agents/a1/home:"+agentHomeRepairMount {
		t.Errorf("helper's only mount is not the agent home: %q", args)
	}
	for _, forbidden := range []string{"DAC_READ_SEARCH", "SYS_ADMIN", "--privileged", "unconfined", "--mount", "--device", "-v ", "--cap-add=ALL", "--userns", "--pid=host", "--network=host"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("helper args contain forbidden %q: %q", forbidden, args)
		}
	}
	var capAdds []string
	for _, a := range args {
		if strings.HasPrefix(a, "--cap-add") {
			capAdds = append(capAdds, a)
		}
	}
	if !slices.Equal(capAdds, []string{"--cap-add=CHOWN", "--cap-add=DAC_OVERRIDE"}) {
		t.Errorf("helper capabilities = %q, want only CHOWN and DAC_OVERRIDE", capAdds)
	}

	// Podman asks for the host user namespace explicitly; nothing else
	// changes.
	podmanArgs, err := agentHomeRepairArgs(testRepairHelper(true))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(podmanArgs, "--userns=host") || len(podmanArgs) != len(args)+1 {
		t.Errorf("podman helper args %q: want the docker args plus --userns=host", podmanArgs)
	}

	for _, tc := range []struct {
		name string
		mut  func(*agentHomeRepairHelper)
	}{
		{"empty home", func(h *agentHomeRepairHelper) { h.req.HomeDir = "" }},
		{"relative home", func(h *agentHomeRepairHelper) { h.req.HomeDir = "home" }},
		{"root", func(h *agentHomeRepairHelper) { h.req.HomeDir = "/" }},
		{"colon", func(h *agentHomeRepairHelper) { h.req.HomeDir = "/a:b" }},
		{"comma", func(h *agentHomeRepairHelper) { h.req.HomeDir = "/a,b" }},
		{"no image", func(h *agentHomeRepairHelper) { h.req.Image = "" }},
		{"option-like image", func(h *agentHomeRepairHelper) { h.req.Image = "--privileged" }},
		{"negative uid", func(h *agentHomeRepairHelper) { h.req.UID = -1 }},
		{"bad name", func(h *agentHomeRepairHelper) { h.name = "agent-x" }},
		{"bad home key", func(h *agentHomeRepairHelper) { h.homeKey = "x" }},
	} {
		h := testRepairHelper(false)
		tc.mut(&h)
		if _, err := agentHomeRepairArgs(h); err == nil {
			t.Errorf("%s: expected an error", tc.name)
		}
	}
}

// The helper's find expression, run directly on a tree, selects
// directories, symlinks and singly-linked files, and skips a regular file
// with a second hard link and a FIFO.
func TestAgentHomeRepairArgs_FindSelection(t *testing.T) {
	if _, err := exec.LookPath("find"); err != nil {
		t.Skip("find not available")
	}
	home := t.TempDir()
	mustWrite := func(p string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(home, ".scion", "hooks", "pre-start.d", "20-harness-provision"))
	mustWrite(filepath.Join(home, "agent-info.json"))
	outside := filepath.Join(t.TempDir(), "outside")
	mustWrite(outside)
	if err := os.Link(outside, filepath.Join(home, "hardlink")); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(home, "symlink")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(home, "fifo"), 0o644); err != nil {
		t.Fatal(err)
	}

	h := testRepairHelper(false)
	h.req = AgentHomeOwnershipRepair{HomeDir: home, Image: "img", UID: os.Getuid(), GID: os.Getgid()}
	args, err := agentHomeRepairArgs(h)
	if err != nil {
		t.Fatal(err)
	}
	findArgs := slices.Clone(args[slices.Index(args, "img")+1:])
	findArgs[0] = home
	// The real expression, chowning to the current ids (a no-op the test
	// process is allowed), succeeds.
	if out, err := exec.Command("find", findArgs...).CombinedOutput(); err != nil {
		t.Fatalf("find with chown failed: %v\n%s", err, out)
	}
	// The same selection, printed.
	sel := append(slices.Clone(findArgs[:slices.Index(findArgs, "-exec")]), "-print")
	out, err := exec.Command("find", sel...).Output()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		rel, _ := filepath.Rel(home, l)
		got = append(got, rel)
	}
	sort.Strings(got)
	want := []string{".", ".scion", ".scion/hooks", ".scion/hooks/pre-start.d", ".scion/hooks/pre-start.d/20-harness-provision", "agent-info.json", "symlink"}
	if !slices.Equal(got, want) {
		t.Errorf("selected %q, want %q", got, want)
	}
}

// The runtime validates the helper's argv against a fail-closed allow-list
// before running it: the built command passes, and any change that adds,
// removes, repeats or alters a flag, the name, the mount, the image or the
// command is refused.
func TestValidateAgentHomeRepairArgs(t *testing.T) {
	for _, userNSHost := range []bool{false, true} {
		h := testRepairHelper(userNSHost)
		args, err := agentHomeRepairArgs(h)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateAgentHomeRepairArgs(args, h); err != nil {
			t.Fatalf("userNSHost=%v: the built helper command is refused: %v", userNSHost, err)
		}
		imageAt := slices.Index(args, "img:1")
		insertBeforeImage := func(extra ...string) []string {
			out := slices.Clone(args[:imageAt])
			out = append(out, extra...)
			return append(out, args[imageAt:]...)
		}
		replace := func(old, new string) []string {
			out := slices.Clone(args)
			out[slices.Index(out, old)] = new
			return out
		}
		without := func(flag string) []string {
			out := slices.Clone(args)
			i := slices.Index(out, flag)
			return slices.Delete(out, i, i+1)
		}
		cases := map[string][]string{
			"privileged":          insertBeforeImage("--privileged"),
			"SYS_ADMIN":           insertBeforeImage("--cap-add=SYS_ADMIN"),
			"DAC_READ_SEARCH":     replace("--cap-add=DAC_OVERRIDE", "--cap-add=DAC_READ_SEARCH"),
			"seccomp unconfined":  insertBeforeImage("--security-opt=seccomp=unconfined"),
			"apparmor unconfined": insertBeforeImage("--security-opt", "apparmor=unconfined"),
			"second mount":        insertBeforeImage("--volume", "/:/host"),
			"--mount":             insertBeforeImage("--mount", "type=bind,src=/,dst=/host"),
			"host network":        replace("--network=none", "--network=host"),
			"no cap-drop":         without("--cap-drop=ALL"),
			"no network flag":     without("--network=none"),
			"no memory limit":     without("--memory=256m"),
			"no cpu limit":        without("--cpus=1"),
			"other memory limit":  replace("--memory=256m", "--memory=0"),
			"repeated cap":        insertBeforeImage("--cap-add=CHOWN"),
			"other name":          replace(testRepairName, "scion-home-repair-ffffffffffffffff"),
			"other entrypoint":    replace("find", "sh"),
			"other home":          replace("/srv/agents/a1/home:"+agentHomeRepairMount, "/srv:"+agentHomeRepairMount),
			"read-only home":      replace("/srv/agents/a1/home:"+agentHomeRepairMount, "/srv/agents/a1/home:"+agentHomeRepairMount+":ro"),
			"other image":         replace("img:1", "img:2"),
			"other owner":         replace("1002:1003", "0:0"),
			"extra command":       append(slices.Clone(args), "-delete"),
			"not run":             replace("run", "exec"),
			"stop-timeout":        insertBeforeImage("--stop-timeout=0"),
			"other label":         replace("scion.helper=agent-home-ownership-"+testRepairKey, "scion.helper=other"),
		}
		if userNSHost {
			cases["no userns"] = without("--userns=host")
		} else {
			cases["userns on docker"] = insertBeforeImage("--userns=host")
		}
		for name, bad := range cases {
			if err := validateAgentHomeRepairArgs(bad, h); err == nil {
				t.Errorf("userNSHost=%v, %s: helper command %q was not refused", userNSHost, name, bad)
			}
		}
	}
	h := testRepairHelper(false)
	args, _ := agentHomeRepairArgs(h)
	h.name = "not-a-helper"
	if err := validateAgentHomeRepairArgs(args, h); err == nil {
		t.Error("a helper name without the prefix and shape was not refused")
	}
	h = testRepairHelper(false)
	h.homeKey = "not-a-key"
	if err := validateAgentHomeRepairArgs(args, h); err == nil {
		t.Error("a home key without the key shape was not refused")
	}
}

func TestDockerRuntime_RepairAgentHomeOwnership(t *testing.T) {
	f := newFakeRepairRuntime(t, rootfulDocker())
	home := newRepairHome(t)
	req := AgentHomeOwnershipRepair{HomeDir: home, Image: "scion-claude:latest", UID: 1002, GID: 1003}
	if err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	name := f.helperName(t)
	if !agentHomeRepairNameRE.MatchString(name) {
		t.Errorf("helper name %q does not have the helper's shape", name)
	}
	want := goldenRepairArgs(home, name, agentHomeRepairKey(home), "scion-claude:latest", "1002:1003", false)
	if got := readLines(t, f.runArgs); !slices.Equal(got, want) {
		t.Errorf("docker args\n got %q\nwant %q", got, want)
	}
	if rm := f.rmLines(t); rm != nil {
		t.Errorf("a successful helper was removed by name (%q); --rm already removes it", rm)
	}
}

// Rootful Podman runs the same validated helper, in the host user
// namespace.
func TestPodmanRuntime_RepairAgentHomeOwnership(t *testing.T) {
	f := newFakeRepairRuntime(t, fakeRepairOpts{info: "false"})
	home := newRepairHome(t)
	req := AgentHomeOwnershipRepair{HomeDir: home, Image: "img", UID: 1002, GID: 1003}
	if err := (&PodmanRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	name := f.helperName(t)
	want := goldenRepairArgs(home, name, agentHomeRepairKey(home), "img", "1002:1003", true)
	got := readLines(t, f.runArgs)
	if !slices.Equal(got, want) {
		t.Errorf("podman args\n got %q\nwant %q", got, want)
	}
	// The allow-list holds on the Podman path too.
	h := agentHomeRepairHelper{req: req, name: name, homeKey: agentHomeRepairKey(home), userNSHost: true}
	if err := validateAgentHomeRepairArgs(got, h); err != nil {
		t.Errorf("podman helper command fails the allow-list: %v", err)
	}
}

// Before a helper starts, helpers left behind for the same agent home (the
// agent runtime died while one ran) are force-removed.
func TestDockerRuntime_RepairAgentHomeOwnershipRemovesLeftoverHelpers(t *testing.T) {
	o := rootfulDocker()
	o.psOut = "0123456789ab\nnot-an-id"
	f := newFakeRepairRuntime(t, o)
	home := newRepairHome(t)
	if err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: home, Image: "img", UID: 1, GID: 1}); err != nil {
		t.Fatal(err)
	}
	if got, want := readLines(t, f.psArgs), []string{"ps", "-aq", "--no-trunc", "--filter", "label=scion.helper=agent-home-ownership-" + agentHomeRepairKey(home)}; !slices.Equal(got, want) {
		t.Errorf("leftover lookup = %q, want %q", got, want)
	}
	if got, want := f.rmLines(t), []string{"rm -f 0123456789ab"}; !slices.Equal(got, want) {
		t.Errorf("leftover removal = %q, want %q (only valid ids)", got, want)
	}
	if !f.ran() {
		t.Error("the helper did not run after the leftover removal")
	}
}

// Rootless Podman, and a Podman whose mode cannot be determined at repair
// time, refuse without running the helper.
func TestPodmanRuntime_RepairAgentHomeOwnershipUnsupportedModes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		rootless  bool
		info      string
		infoFails bool
		want      string
	}{
		{"rootless at construction", true, "false", false, "(rootless podman)"},
		{"rootless at repair time", false, "true", false, "(rootless podman)"},
		{"detection fails", false, "", true, "podman mode undetectable"},
		{"empty answer", false, "", false, "podman mode undetectable"},
		{"unexpected answer", false, "maybe", false, "podman mode undetectable"},
	} {
		f := newFakeRepairRuntime(t, fakeRepairOpts{info: tc.info, infoFails: tc.infoFails})
		r := &PodmanRuntime{Command: f.bin, Rootless: tc.rootless}
		err := r.RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1})
		if !errors.Is(err, ErrAgentHomeRepairUnsupported) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want ErrAgentHomeRepairUnsupported naming %s", tc.name, err, tc.want)
		}
		if f.ran() {
			t.Errorf("%s: the helper ran", tc.name)
		}
	}
}

// Rootless Docker, Docker with user-namespace remapping and a Docker whose
// mode cannot be determined refuse without running the helper.
func TestDockerRuntime_RepairAgentHomeOwnershipUnsupportedModes(t *testing.T) {
	for _, tc := range []struct {
		info      string
		infoFails bool
		want      string
	}{
		{`["name=seccomp,profile=builtin","name=rootless","name=cgroupns"]`, false, "(rootless docker)"},
		{`["name=apparmor","name=seccomp,profile=builtin","name=userns"]`, false, "(docker userns-remap)"},
		{"", true, "docker mode undetectable"},
		// The probe answer is parsed strictly: anything but a non-empty
		// JSON array of name=... entries refuses.
		{"", false, "docker mode undetectable"},
		{"not json", false, "docker mode undetectable"},
		{`["name=seccomp,profile=builtin"`, false, "docker mode undetectable"},
		{`[]`, false, "docker mode undetectable"},
		{`null`, false, "docker mode undetectable"},
		{`["seccomp"]`, false, "docker mode undetectable"},
		{`{"name":"rootless"}`, false, "docker mode undetectable"},
		// Options after a comma do not hide the mode.
		{`["name=seccomp,profile=builtin","name=userns,foo=bar"]`, false, "(docker userns-remap)"},
		{`["name=rootless,x=y"]`, false, "(rootless docker)"},
		// An empty name, with or without options, is unexpected.
		{`["name=,x"]`, false, "docker mode undetectable"},
		{`["name="]`, false, "docker mode undetectable"},
		// One oversized entry not of the form name=... is quoted truncated.
		{`["` + strings.Repeat("e", 5000) + `"]`, false, "(5000 bytes total)"},
	} {
		f := newFakeRepairRuntime(t, fakeRepairOpts{info: tc.info, infoFails: tc.infoFails})
		err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1})
		if !errors.Is(err, ErrAgentHomeRepairUnsupported) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("info %q: err = %v, want ErrAgentHomeRepairUnsupported naming %s", tc.info, err, tc.want)
		}
		if f.ran() {
			t.Errorf("info %q: the helper ran", tc.info)
		}
	}
}

// A failed helper run is removed by name and reported with its output.
func TestDockerRuntime_RepairAgentHomeOwnershipFailureRemovesHelper(t *testing.T) {
	o := rootfulDocker()
	o.runExit = 3
	f := newFakeRepairRuntime(t, o)
	err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1})
	if err == nil || !strings.Contains(err.Error(), "helper-output") || !strings.Contains(err.Error(), "was removed") {
		t.Fatalf("expected the helper failure with its output and its removal, got %v", err)
	}
	if got, want := f.rmLines(t), []string{"rm -f " + f.helperName(t)}; !slices.Equal(got, want) {
		t.Errorf("cleanup = %q, want %q", got, want)
	}
}

// When the removal itself fails, the error says so instead of claiming it.
func TestDockerRuntime_RepairAgentHomeOwnershipFailedRemovalReported(t *testing.T) {
	o := rootfulDocker()
	o.runExit = 3
	o.rmFails = true
	f := newFakeRepairRuntime(t, o)
	err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1})
	if err == nil || !strings.Contains(err.Error(), "removal failed") || strings.Contains(err.Error(), "was removed") {
		t.Fatalf("err = %v, want it to report the failed removal", err)
	}
}

// A helper that outlives the timeout is removed by name: killing the CLI
// alone would leave the container running.
func TestDockerRuntime_RepairAgentHomeOwnershipTimeoutRemovesHelper(t *testing.T) {
	old := agentHomeRepairTimeout
	agentHomeRepairTimeout = 300 * time.Millisecond
	t.Cleanup(func() { agentHomeRepairTimeout = old })
	o := rootfulDocker()
	o.runSleep = 10
	f := newFakeRepairRuntime(t, o)
	start := time.Now()
	err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1})
	if err == nil || !strings.Contains(err.Error(), "timed out") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("repair returned after %s, want shortly after the timeout", elapsed)
	}
	name := f.helperName(t)
	if got, want := f.rmLines(t), []string{"rm -f " + name}; !slices.Equal(got, want) {
		t.Errorf("cleanup = %q, want %q", got, want)
	}
	if !strings.Contains(err.Error(), name) {
		t.Errorf("error %q does not name the helper %s", err, name)
	}
}

// When the caller's context is cancelled mid-run, the helper is still
// removed by name (the cleanup does not use the cancelled context), and the
// error says it was cancelled.
func TestDockerRuntime_RepairAgentHomeOwnershipCancelRemovesHelper(t *testing.T) {
	o := rootfulDocker()
	o.runSleep = 10
	f := newFakeRepairRuntime(t, o)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		// Cancel once the run has started.
		for i := 0; i < 500 && !f.ran(); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(ctx, AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1})
	if err == nil || !strings.Contains(err.Error(), "cancelled") || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want a cancellation error", err)
	}
	if got, want := f.rmLines(t), []string{"rm -f " + f.helperName(t)}; !slices.Equal(got, want) {
		t.Errorf("cleanup = %q, want %q", got, want)
	}
}

// The helper's agent home goes through the agent-home and recursive-chown
// root checks before anything runs: a system path, $HOME, a non-home path
// under ~/.scion, and a symlink to one, are refused without running the
// helper; real agent-home shapes pass, and the helper mounts the resolved
// path.
func TestRepairAgentHomeOwnership_ValidatesHome(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	nonHome := filepath.Join(tmpHome, ".scion", "projects", "p")
	if err := os.MkdirAll(nonHome, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "agents", "a1", "home")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nonHome, link); err != nil {
		t.Fatal(err)
	}
	for name, home := range map[string]string{
		"/etc":                       "/etc",
		"$HOME":                      tmpHome,
		"non-home path under .scion": nonHome,
		"symlink to it":              link,
	} {
		f := newFakeRepairRuntime(t, rootfulDocker())
		err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: home, Image: "img", UID: 1, GID: 1})
		if err == nil {
			t.Errorf("%s: repair of %q was not refused", name, home)
		}
		if f.ran() {
			t.Errorf("%s: the helper ran", name)
		}
	}

	for _, rel := range [][]string{
		{".scion", "agents", "a", "home"},
		{".scion", "projects", "p", ".scion", "agents", "a", "home"},
		{".scion", "project-configs", "d__1", ".scion", "agents", "a", "home"},
	} {
		home := filepath.Join(append([]string{tmpHome}, rel...)...)
		if err := os.MkdirAll(home, 0o755); err != nil {
			t.Fatal(err)
		}
		// Reach it through a symlinked parent: the helper mounts the
		// resolved path.
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Symlink(filepath.Dir(home), alias); err != nil {
			t.Fatal(err)
		}
		f := newFakeRepairRuntime(t, rootfulDocker())
		err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: filepath.Join(alias, "home"), Image: "img", UID: 1, GID: 1})
		if err != nil {
			t.Errorf("%s: real agent home refused: %v", home, err)
			continue
		}
		resolved, _ := filepath.EvalSymlinks(home)
		args := readLines(t, f.runArgs)
		if i := slices.Index(args, "--volume"); i < 0 || args[i+1] != resolved+":"+agentHomeRepairMount {
			t.Errorf("%s: helper does not mount the resolved home: %q", home, args)
		}
	}
}

// The repair target and SCION_HOST_UID/GID come from the same function:
// for every backend, the ids buildCommonRunArgs advertises are
// AdvertisedHostOwnerIDs'.
func TestAdvertisedHostOwnerIDs_MatchesRunArgs(t *testing.T) {
	for _, tc := range []struct {
		backend          string
		nfsUID, nfsGID   int
		wantUID, wantGID int
	}{
		{"", 0, 0, os.Getuid(), os.Getgid()},
		{"local", 2000, 2000, os.Getuid(), os.Getgid()},
		{"nfs", 0, 0, 1000, 1000},
		{"nfs", 2001, 2002, 2001, 2002},
	} {
		uid, gid := AdvertisedHostOwnerIDs(tc.backend, tc.nfsUID, tc.nfsGID)
		if uid != tc.wantUID || gid != tc.wantGID {
			t.Errorf("AdvertisedHostOwnerIDs(%q, %d, %d) = %d:%d, want %d:%d", tc.backend, tc.nfsUID, tc.nfsGID, uid, gid, tc.wantUID, tc.wantGID)
		}
		cfg := minimalRunConfig()
		cfg.WorkspaceBackendName, cfg.NFSUID, cfg.NFSGID = tc.backend, tc.nfsUID, tc.nfsGID
		args, err := buildCommonRunArgs(cfg)
		if err != nil {
			t.Fatalf("buildCommonRunArgs(%q): %v", tc.backend, err)
		}
		assertEnvInArgs(t, args, fmt.Sprintf("SCION_HOST_UID=%d", uid), "advertised uid")
		assertEnvInArgs(t, args, fmt.Sprintf("SCION_HOST_GID=%d", gid), "advertised gid")
	}
}

// captureRuntimeLog sends runtimeLog to a buffer for the rest of the test.
func captureRuntimeLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	prev := runtimeLog
	runtimeLog = slog.New(slog.NewTextHandler(&logs, nil))
	t.Cleanup(func() { runtimeLog = prev })
	return &logs
}

// The leftover sweep is cleanup only: when listing fails, an id is not a
// container id, or removing leftovers fails, it logs and the repair still
// runs.
func TestDockerRuntime_RepairAgentHomeOwnershipSweepFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mut     func(*fakeRepairOpts)
		wantRm  []string
		wantLog string
	}{
		{"listing fails", func(o *fakeRepairOpts) { o.psFails = true }, nil, "Could not list leftover"},
		{"invalid id", func(o *fakeRepairOpts) { o.psOut = "not-an-id" }, nil, "Ignoring unexpected leftover"},
		{"overlong invalid id", func(o *fakeRepairOpts) { o.psOut = strings.Repeat("z", 64) + "TAILTAIL" }, nil, "(72 bytes total)"},
		{"invalid ids mixed with a valid one", func(o *fakeRepairOpts) { o.psOut = "bad1\n0123456789ab\nbad2\nbad3\nbad4" }, []string{"rm -f 0123456789ab"}, "count=4"},
		{"removal fails", func(o *fakeRepairOpts) { o.psOut = "0123456789ab"; o.rmFails = true }, []string{"rm -f 0123456789ab"}, "Could not remove leftover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureRuntimeLog(t)
			o := rootfulDocker()
			tc.mut(&o)
			f := newFakeRepairRuntime(t, o)
			if err := (&DockerRuntime{Command: f.bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1}); err != nil {
				t.Fatalf("repair failed: %v", err)
			}
			if !f.ran() {
				t.Error("the helper did not run")
			}
			if got := f.rmLines(t); !slices.Equal(got, tc.wantRm) {
				t.Errorf("rm calls = %q, want %q", got, tc.wantRm)
			}
			if !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("log %q lacks %q", logs.String(), tc.wantLog)
			}
			// Rejected ids are reported in one warning, at most three of
			// them quoted.
			if n := strings.Count(logs.String(), "Ignoring unexpected leftover"); n > 1 {
				t.Errorf("rejected ids logged in %d warnings, want 1: %s", n, logs.String())
			}
			if strings.Contains(logs.String(), "TAILTAIL") {
				t.Errorf("log quotes the tail of an overlong rejected id: %s", logs.String())
			}
			if strings.Contains(logs.String(), "bad4") {
				t.Errorf("log quotes more than the first three rejected ids: %s", logs.String())
			}
		})
	}
}

// An oversized probe answer is quoted truncated, with its total size, by
// both runtimes' refusals.
func TestRepairAgentHomeOwnership_OversizedProbeAnswerTruncated(t *testing.T) {
	big := strings.Repeat("x", 5000)
	for name, repair := range map[string]func(bin string) error{
		"docker": func(bin string) error {
			return (&DockerRuntime{Command: bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1})
		},
		"podman": func(bin string) error {
			return (&PodmanRuntime{Command: bin}).RepairAgentHomeOwnership(context.Background(), AgentHomeOwnershipRepair{HomeDir: newRepairHome(t), Image: "img", UID: 1, GID: 1})
		},
	} {
		f := newFakeRepairRuntime(t, fakeRepairOpts{info: big})
		err := repair(f.bin)
		if !errors.Is(err, ErrAgentHomeRepairUnsupported) || !strings.Contains(err.Error(), "mode undetectable") {
			t.Fatalf("%s: err = %v, want an undetectable-mode refusal", name, err)
		}
		if !strings.Contains(err.Error(), "(5000 bytes total)") || len(err.Error()) > 1000 {
			t.Errorf("%s: error not truncated with the total size (%d bytes): %.300s", name, len(err.Error()), err)
		}
		if f.ran() {
			t.Errorf("%s: the helper ran", name)
		}
	}
}

func TestTruncateForMessage(t *testing.T) {
	if got := truncateForMessage("short", 10); got != "short" {
		t.Errorf("short input changed: %q", got)
	}
	if got := truncateForMessage("abcd", 4); got != "abcd" {
		t.Errorf("input at the limit changed: %q", got)
	}
	if got := truncateForMessage("", 4); got != "" {
		t.Errorf("empty input changed: %q", got)
	}
	if got := truncateForMessage("abcdefghijkl", 4); got != "abcd... (12 bytes total)" {
		t.Errorf("truncated = %q", got)
	}
}
