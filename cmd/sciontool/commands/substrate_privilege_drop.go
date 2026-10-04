/*
Copyright 2026 The Scion Authors.
*/

package commands

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/substratecaps"
)

// privilegeDropPreconditionDeps groups checkPrivilegeDropFeasible's external
// dependencies so tests can substitute all of them, rather than depending on
// the real capability set, "scion" user, or process environment a unit test
// runs in — the same reasoning as requirePrivilegeDropOrFail's (init.go)
// separation from setupHostUser.
type privilegeDropPreconditionDeps struct {
	// hasCapBit checks one capability bit (see substratecaps.Capability.
	// EffBit) at a time, rather than one bool field per capability, so
	// checkPrivilegeDropFeasible can iterate substratecaps.Required in
	// full without this struct having to grow a field — and a test having
	// to remember to fill it in — every time that list does.
	hasCapBit  func(bit uint) bool
	lookupUser func(string) (*user.User, error)
	getenv     func(string) string

	// statPath reads a path's mode, owning uid and owning gid, without
	// following through to any deeper access check (see canSearchDir/
	// homeOwnedAndWritable, both in substrate_rootfs.go). Injectable so the
	// traversability checks below can be driven against a fake rootfs in
	// tests instead of the real '/' and $HOME.
	statPath func(string) (fs.FileInfo, error)
}

// defaultPrivilegeDropPreconditionDeps wires checkPrivilegeDropFeasible to
// the real process: /proc/self/status, the real "scion" user, the real
// environment, and the real filesystem.
var defaultPrivilegeDropPreconditionDeps = privilegeDropPreconditionDeps{
	hasCapBit: hasCapBit,
	// lookupUser wraps the scionUserLookup var (init.go) in a closure, not
	// its current value, so TestMain's override (applied after this struct
	// is initialized at package-init time) still takes effect — putting
	// this checker under the same two defenses as every other "scion"
	// lookup in cmd/sciontool/commands: TestMain's stub, and
	// defaultScionUserLookup's own testing.Testing() gate.
	lookupUser: func(username string) (*user.User, error) { return scionUserLookup(username) },
	getenv:     os.Getenv,
	statPath:   os.Stat,
}

// errPrivilegeDropPrecondition is checkPrivilegeDropFeasible's only error:
// deliberately generic and secret-free, since it crosses into
// pkg/sciontool/substrate's HTTP response body (see PrivilegeDropChecker's
// doc comment) rather than staying in a local log line. Only the
// setuid-root-sudo branch below logs which specific condition failed; every
// other branch (a missing capability, an unresolvable "scion" user, missing/
// unparseable SCION_HOST_UID/GID, or a traversability/ownership failure)
// returns this same sentinel with no accompanying log line. Reached through
// substrateServePrivilegeDropChecker (substrate_serve.go), which just
// returns this error unchanged — it is handleBootstrap
// (pkg/sciontool/substrate/server.go) that actually logs this generic
// string, at its one call site, the /bootstrap HTTP precondition.
var errPrivilegeDropPrecondition = errors.New("privilege drop precondition not met: a required capability, the scion user, or SCION_HOST_UID/GID were not all available")

// checkPrivilegeDropFeasible is substrate-serve's synchronous /bootstrap
// precondition (pkg/sciontool/substrate.PrivilegeDropChecker): it lets
// handleBootstrap refuse the request itself, before it ever responds 200,
// so a caller that can't actually drop privileges gets Run() returning an
// error and the actor deleted, the same way any other bootstrap failure
// does — rather than a harness that silently never starts inside an actor
// the broker still believes is running. It must be cheap and side-effect-
// free — no sed, no usermod, no chmod/chown — so it deliberately does not
// reimplement setupHostUser's realignment or fixupRootfsForScion's own
// fixup; it only re-checks the conditions that can each independently make
// either of those silently produce nothing usable:
//   - every capability in substratecaps.Required effective — not just
//     SETUID/SETGID: a template built without one of them (e.g. CHOWN)
//     must fail here, synchronously, rather than pass this check and die
//     deep inside RunInit once the harness is already supposed to be
//     starting (observed live — see substratecaps.Required's CHOWN entry
//     for the exact log lines);
//   - the "scion" user resolvable at all;
//   - SCION_HOST_UID/GID present and parseable (buildBootstrapEnv sets these
//     into req.Env, applied to the process environment by handleBootstrap
//     just before this runs — see substrate_bootstrap.go);
//   - the scion user can actually reach and use its own home directory:
//     '/', every parent of $HOME and every parent of the workspace path
//     traversable by it, and $HOME itself owned by it and writable by it.
//     fixupRootfsForScion (called at substrate-serve startup, and again
//     here as a fallback via RootfsFixup) is what's supposed to guarantee
//     this; this check is what catches it not having (an actor that never
//     went through that startup path, or a rootfs oddity fixupRootfsForScion
//     doesn't yet cover). Traversability is computed from each directory's
//     mode/uid/gid, never by actually attempting to switch to the scion
//     user — see canSearchDir (substrate_rootfs.go).
//
// This does not guarantee setupHostUser's usermod/sed realignment will
// succeed (e.g. a corrupted /etc/passwd could still fail it) — that residual
// gap is exactly why requirePrivilegeDropOrFail stays as defence in depth in
// RunInit itself (init.go).
func checkPrivilegeDropFeasible(d privilegeDropPreconditionDeps) error {
	for _, c := range substratecaps.Required {
		if !d.hasCapBit(c.EffBit) {
			return errPrivilegeDropPrecondition
		}
	}
	scionUser, err := d.lookupUser("scion")
	if err != nil {
		return errPrivilegeDropPrecondition
	}
	hostUID, hostGID := d.getenv("SCION_HOST_UID"), d.getenv("SCION_HOST_GID")
	if hostUID == "" || hostGID == "" {
		return errPrivilegeDropPrecondition
	}
	if _, err := strconv.Atoi(hostUID); err != nil {
		return errPrivilegeDropPrecondition
	}
	if _, err := strconv.Atoi(hostGID); err != nil {
		return errPrivilegeDropPrecondition
	}

	uid64, uidErr := strconv.ParseUint(scionUser.Uid, 10, 32)
	gid64, gidErr := strconv.ParseUint(scionUser.Gid, 10, 32)
	if uidErr != nil || gidErr != nil {
		return errPrivilegeDropPrecondition
	}
	uid, gid := uint32(uid64), uint32(gid64)

	workspacePath := d.getenv("SCION_WORKSPACE_PATH")
	if workspacePath == "" {
		workspacePath = "/workspace"
	}

	dirsToTraverse := mergeDirLists([]string{"/"}, parentDirs(scionUser.HomeDir), parentDirs(workspacePath))
	for _, dir := range dirsToTraverse {
		info, err := d.statPath(dir)
		if err != nil || !canSearchDir(info, uid, gid) {
			return errPrivilegeDropPrecondition
		}
	}

	homeInfo, err := d.statPath(scionUser.HomeDir)
	if err != nil || !homeOwnedAndWritable(homeInfo, uid) {
		return errPrivilegeDropPrecondition
	}

	if path := findSetuidRootSudo(d.statPath); path != "" {
		log.Error("privilege-drop precondition: %s is setuid-root; the rootfs fixup should have stripped this", path)
		return errPrivilegeDropPrecondition
	}

	return nil
}

// findSetuidRootSudo reports the first path (if any) among sudoCheckDirs
// (see substrate_rootfs.go) whose "sudo" binary is still setuid-root.
// statPath follows symlinks (the real destination's mode is what matters,
// the same "resolve, then check the real thing" shape rootexec.Resolve
// uses), so this catches a setuid binary reached through any legitimate
// symlink chain, not only a direct regular file.
//
// Deliberately does NOT parse /etc/sudoers, /etc/sudoers.d, "#include"
// directives, or sudo/wheel group membership: after stripSudoSetuidBits
// runs (both rootfs-fixup call sites, every actor start), no binary here
// should ever be setuid-root, so this exists purely to catch a stale golden
// template — or any other path — that skipped that fixup, not to
// reimplement sudo's own authorization model.
func findSetuidRootSudo(statPath func(string) (fs.FileInfo, error)) string {
	for _, dir := range sudoCheckDirs {
		candidate := filepath.Join("/", dir, "sudo")
		info, err := statPath(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			// Never-fail-open: a stat error other than "not there at all"
			// (EACCES, ELOOP, EIO, ...) means this candidate's real state
			// could not be determined, which this precondition treats the
			// same as finding a problem, not as "assume it's fine".
			return candidate
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return candidate
		}
		if info.Mode()&os.ModeSetuid != 0 && stat.Uid == 0 {
			return candidate
		}
	}
	return ""
}

// hasCapBit is hasCapSetUID's (init.go) generalization to an arbitrary
// capability bit (see substratecaps.Capability.EffBit), used by
// checkPrivilegeDropFeasible to verify substratecaps.Required in full —
// every required capability, not just SETUID — without a hardcoded function
// per capability. Shares parseCapBit (init.go) with hasCapSetUID so the two
// can never drift in how they read /proc/self/status.
func hasCapBit(bit uint) bool {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	return parseCapBit(string(data), bit)
}
