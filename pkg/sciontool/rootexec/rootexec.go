/*
Copyright 2026 The Scion Authors.
*/

// Package rootexec is the one place every root-context subprocess in
// sciontool resolves a bare command name and builds its environment.
//
// On substrate, root is a security boundary, but PID 1's own inherited PATH
// includes a directory the workload owns outright:
// "/usr/local/share/npm-global/bin" is chowned to the workload uid so the
// harness's own npm-installed tools can be found on PATH — and it sits
// ahead of "/usr/bin" on that PATH. A root process that execs a bare name
// like "sh", "iptables", "git", or "su" and lets the kernel/libc (or Go's
// os/exec, which reproduces the same search using os.Getenv("PATH")) walk
// that PATH can be handed a binary the workload planted there, and run it
// as root.
//
// Resolve fixes this by never consulting the ambient PATH at all: it
// searches a fixed, hardcoded list of directories that hold only
// image-installed, root-owned content, and independently verifies — by
// fd-walk, not by trusting the directory names — that both the resolved
// binary and every real directory leading to it are owned by uid 0 (or, on
// a runtime with no separate root/workload identity at all, by this
// process's own uid) and not group- or other-writable. Env builds the
// companion environment for the resulting exec.Cmd from scratch, so nothing
// PID 1 happened to inherit (another PATH, an LD_PRELOAD, a BASH_ENV) can
// steer it either.
package rootexec

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/dirfd"
)

// SearchPath is the fixed, hardcoded list of directories Resolve searches,
// in order — the standard root PATH (Debian's default, and the same order
// pkg/runtime's own Cloud Run sandbox runtime already uses), not a
// hand-picked subset: "/usr/local/sbin" and "/usr/local/bin" are included
// because a real scion agent image installs some root-owned tools (git,
// among others) only there, and excluding them silently broke resolution
// on exactly those images. Including them is still safe: Resolve's fd-walk
// verification requires every directory in the chain, and the binary
// itself, to be root-owned (or, on a runtime with no separate root/
// workload identity at all, self-owned) and free of the group- and
// other-write bits — a workload-owned or group-writable "/usr/local/bin"
// (e.g. the historical Debian "root:staff 2775" shape) is refused, not
// silently trusted because of its name.
//
// It deliberately does not include "/opt/scion/bin": that directory's
// ownership varies by how a given image stages sciontool itself, and a
// caller that needs to re-invoke this same binary should resolve it via
// "/proc/self/exe" (the running inode) instead of a PATH search — see this
// package's doc comment and cmd/sciontool/commands' reExecWithCleanEnv for
// why a path-based re-exec of "myself" is its own, distinct hazard a fixed
// search list does not solve. Also excluded: "/usr/local/share/npm-global/
// bin" (workload-owned by design), "/usr/local/go/bin", and any gcloud SDK
// bin directory.
//
// This list is intentionally NOT os.Getenv("PATH") and Resolve never reads
// that variable: every entry here is a location every supported image is
// expected to install only root-owned content into — verified per call, not
// assumed from the name — regardless of whether that image also happens to
// merge "/bin" into "/usr/bin" (Resolve's verification tolerates that; see
// VerifyRootOwnedExecutable — the directories named here are what a caller
// may point PATH-free tooling at, not a claim that each is a distinct
// inode). This is also the list the sudo-hardening precondition
// (cmd/sciontool/commands' findSetuidRootSudo) scans, via SudoCheckDirs
// below, so the two can never drift apart.
var SearchPath = []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}

// SudoCheckDirs returns SearchPath's entries as directories relative to a
// filesystem root rather than absolute paths, for callers (the sudo
// bootstrap fixup and its bootstrap precondition) that need to join it
// against a root other than "/" (a test fixture) or that already work in
// terms of relative directory names. A function, not a precomputed slice,
// so it always reflects SearchPath's current value rather than whatever it
// was at package-init time. Sharing this, rather than each caller
// hardcoding its own copy of the same directory names, is what keeps the
// fixup's own strip and the precondition's own check from silently
// drifting onto two different lists.
func SudoCheckDirs() []string {
	dirs := make([]string, len(SearchPath))
	for i, d := range SearchPath {
		dirs[i] = strings.TrimPrefix(d, "/")
	}
	return dirs
}

// errNotBareName is returned by Resolve when name is empty or already
// contains a path separator — Resolve exists to turn a bare name into a
// safe absolute path, not to re-verify a path a caller already built.
var errNotBareName = errors.New("rootexec: not a bare command name")

// Resolve searches SearchPath, in order, for name, and returns the first
// candidate — e.g. "/usr/sbin/iptables" — whose entire symlink chain (if
// any) and final destination verify as trusted: every real directory along
// the way, each symlink hop's own owner, and the destination binary itself,
// owned by uid 0 (or this process's own euid — see dirfd's chainIsTrusted
// for why) and free of the group- and other-write bits.
//
// Resolve returns the CANDIDATE path, never the resolved destination.
// Several binaries this package resolves (Debian's "iptables", reached
// through "/etc/alternatives/iptables" to a "xtables-nft-multi" multi-call
// binary; some coreutils/busybox builds behave the same way for "whoami",
// "sh", and others) decide their own behavior from argv[0]'s basename. A
// caller that went on to exec the fully-resolved destination instead of
// the candidate would hand such a binary the wrong argv[0] and get a
// dispatch error, even though the exact same inode ends up running either
// way — so the verification (dirfd.VerifyRootOwnedExecutable) checks the
// whole chain hop by hop without ever collapsing it to a single resolved
// path, and Resolve hands the caller back the one path whose basename is
// guaranteed to be the name it asked for.
//
// Fails closed: if name is not found as a trusted executable under any
// SearchPath entry, Resolve returns an error and the caller must not exec
// anything — there is no fallback to $PATH or to any other location.
func Resolve(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("%w: %q", errNotBareName, name)
	}

	var errs []error
	for _, dir := range SearchPath {
		candidate := filepath.Join(dir, name)
		if err := dirfd.VerifyRootOwnedExecutable(candidate); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", candidate, err))
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("rootexec: %q not found as a trusted executable under %s: %w",
		name, strings.Join(SearchPath, ":"), errors.Join(errs...))
}

// Env builds a hardened environment for a root-context exec.Cmd: the fixed
// PATH from SearchPath, plus whatever explicit "KEY=VALUE" pairs the caller
// supplies. Nothing else is carried over — no LD_*, BASH_ENV, ENV, IFS,
// GIT_*, PYTHON*, or anything else PID 1 happens to have inherited — a
// caller that genuinely needs one of those must state it explicitly in
// extra, the way cmd/sciontool/commands' checkWorkspaceGit states HOME for
// its own git child.
func Env(extra ...string) []string {
	env := make([]string, 0, len(extra)+1)
	env = append(env, "PATH="+strings.Join(SearchPath, ":"))
	env = append(env, extra...)
	return env
}

// SelfExe returns the path a root process must use to re-exec itself:
// "/proc/self/exe", the kernel's magic symlink to the running inode. Unlike
// os.Executable() (which re-reads a path from disk that may itself sit
// somewhere workload-writable, and so could resolve to a binary planted
// there since this process started) or a PATH search (Resolve is for
// external tools, and deliberately excludes any directory this binary might
// itself be staged in — see SearchPath's doc comment), execve on
// "/proc/self/exe" always re-execs the exact in-memory image already
// running, regardless of what — if anything — now sits at its on-disk path.
func SelfExe() string {
	return "/proc/self/exe"
}
