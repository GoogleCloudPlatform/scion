/*
Copyright 2026 The Scion Authors.
*/

package hooks

import "testing"

func rootNode() NodeOwnership { return NodeOwnership{UID: 0, Perm: 0o755} }

func TestDecideExecAsRoot_AllRootOwnedNonWritable(t *testing.T) {
	chain := []NodeOwnership{rootNode(), rootNode(), rootNode()}
	if !DecideExecAsRoot(rootNode(), chain) {
		t.Fatal("expected root-eligible chain to run as root")
	}
}

func TestDecideExecAsRoot_ScriptNotRootOwned(t *testing.T) {
	script := NodeOwnership{UID: 1000, Perm: 0o755}
	chain := []NodeOwnership{rootNode(), rootNode()}
	if DecideExecAsRoot(script, chain) {
		t.Fatal("expected non-root-owned script to be dropped")
	}
}

func TestDecideExecAsRoot_ScriptGroupWritable(t *testing.T) {
	script := NodeOwnership{UID: 0, Perm: 0o775} // group-writable
	chain := []NodeOwnership{rootNode()}
	if DecideExecAsRoot(script, chain) {
		t.Fatal("expected group-writable script to be dropped")
	}
}

func TestDecideExecAsRoot_ScriptWorldWritable(t *testing.T) {
	script := NodeOwnership{UID: 0, Perm: 0o757} // world-writable
	chain := []NodeOwnership{rootNode()}
	if DecideExecAsRoot(script, chain) {
		t.Fatal("expected world-writable script to be dropped")
	}
}

func TestDecideExecAsRoot_DirNotRootOwned(t *testing.T) {
	chain := []NodeOwnership{rootNode(), {UID: 1000, Perm: 0o755}, rootNode()}
	if DecideExecAsRoot(rootNode(), chain) {
		t.Fatal("expected a non-root-owned ancestor directory to force a drop")
	}
}

func TestDecideExecAsRoot_DirGroupOrWorldWritable(t *testing.T) {
	tests := []struct {
		name string
		perm uint32
	}{
		{"group-writable", 0o775},
		{"world-writable", 0o757},
		{"both", 0o777},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chain := []NodeOwnership{rootNode(), {UID: 0, Perm: tc.perm}}
			if DecideExecAsRoot(rootNode(), chain) {
				t.Fatalf("expected a %s ancestor directory to force a drop", tc.name)
			}
		})
	}
}

func TestDecideExecAsRoot_EmptyChainStillChecksScript(t *testing.T) {
	if DecideExecAsRoot(NodeOwnership{UID: 1000, Perm: 0o755}, nil) {
		t.Fatal("expected a non-root script with an empty chain to be dropped")
	}
	if !DecideExecAsRoot(rootNode(), nil) {
		t.Fatal("expected a root-owned, non-writable script with an empty chain to run as root")
	}
}

func TestDecideExecAsRoot_SetuidOrOtherBitsDoNotOverrideWriteCheck(t *testing.T) {
	// A setuid/sticky bit alongside a group/world write bit must still drop
	// -- Perm carries the full low-12 bits, but only the write bits gate the
	// decision.
	script := NodeOwnership{UID: 0, Perm: 0o4755} // setuid, not group/world-writable
	if !DecideExecAsRoot(script, nil) {
		t.Fatal("expected setuid-but-not-writable script to still be root-eligible")
	}
}
