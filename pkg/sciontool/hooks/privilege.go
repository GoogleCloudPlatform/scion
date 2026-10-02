/*
Copyright 2026 The Scion Authors.
*/

package hooks

// modeGroupWrite and modeOtherWrite are the traditional Unix permission bit
// positions for "group can write" and "other can write" — the low 12 bits a
// raw stat(2) call reports (e.g. unix.Stat_t.Mode & 0o7777), not os.FileMode's
// own encoding, which shifts setuid/setgid/sticky to different bit
// positions. Every caller of NodeOwnership must pass raw stat mode bits.
const (
	modeGroupWrite = 0o020
	modeOtherWrite = 0o002
)

// NodeOwnership is one fstat result along a hook script's path: either the
// script file itself, or one directory in the chain from the script's
// parent up to the filesystem root. UID and Perm are populated purely from
// fstat on an already-opened, symlink-verified file descriptor — never
// re-derived from a path lookup — so DecideExecAsRoot can be driven by a
// test with fabricated values, without root and without touching a real
// filesystem.
type NodeOwnership struct {
	// UID is the node's owning user ID (st_uid).
	UID uint32
	// Perm is the low 12 permission bits of the node's mode (st_mode &
	// 0o7777) — read/write/execute for owner/group/other plus
	// setuid/setgid/sticky. Only the group- and other-write bits are
	// currently inspected, but the full field is kept so a future check
	// (e.g. refusing setuid/setgid hook scripts) doesn't need a second,
	// differently-shaped type threaded through every call site.
	Perm uint32
}

// rootProtected reports whether a single node (the script file, or one
// directory in its chain) is safe to trust with root execution: owned by
// uid 0 and not writable by its group or by everyone. This says nothing
// about any OTHER node in the chain — DecideExecAsRoot is what combines
// every node's result into the final yes/no.
func (n NodeOwnership) rootProtected() bool {
	return n.UID == 0 && n.Perm&(modeGroupWrite|modeOtherWrite) == 0
}

// DecideExecAsRoot is the privilege-drop-enforced-mode root/drop decision, a
// pure function of the fstat results for a hook script (script) and every
// directory from its immediate parent up to and including "/" (chain, in any
// order — each entry is checked independently and unconditionally, so the
// caller's ordering does not affect the result).
//
// A hook script runs as root only if the script file AND every directory in
// its chain are owned by uid 0 and are not group- or world-writable. Any
// other outcome — a non-root owner anywhere in the chain, or a group/world
// write bit anywhere in the chain or on the script itself — means the
// script must run dropped to the workload identity instead.
//
// There is no per-event exception: this same function decides pre-start,
// post-start, pre-stop, and session-end hooks alike. A caller must never
// special-case an event name to skip this check — that would reopen exactly
// the hole this function exists to close (an event that fires after the
// workload has control, over a script the workload can plant in a directory
// it owns).
func DecideExecAsRoot(script NodeOwnership, chain []NodeOwnership) bool {
	if !script.rootProtected() {
		return false
	}
	for _, dir := range chain {
		if !dir.rootProtected() {
			return false
		}
	}
	return true
}
