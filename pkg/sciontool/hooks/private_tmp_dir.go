/*
Copyright 2026 The Scion Authors.
*/

package hooks

// PrivateRootTmpDir is the dedicated, root-owned directory root uses to
// stage content it must both write and read back before installing the
// result into a workload-owned location (see cmd/sciontool/commands'
// configureSharedWorkspaceGit). Never $TMPDIR or the system temp directory:
// on a runtime where root is a security boundary, that directory can be
// world-writable with no sticky bit, which lets the workload rename any
// entry out of it and plant a symlink in its place while root is still
// using it.
//
// A package var, not a const, purely so a test can point it at a throwaway
// directory instead of the real, root-owned "/run/scion/tmp". Production
// code never reassigns it.
var PrivateRootTmpDir = "/run/scion/tmp"

// PrivateRootTmpDirMode is the mode PrivateRootTmpDir is created and kept
// at: root (or, in a rootless deployment, the caller's own uid — see
// dirfd.EnsureDirNoFollowRootOwned) only, no group or other access at all.
// This directory holds a private working copy of files that must never be
// listable, let alone enterable, by anything else.
const PrivateRootTmpDirMode = 0o700
