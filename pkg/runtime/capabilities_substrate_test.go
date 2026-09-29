/*
Copyright 2026 The Scion Authors.
*/

package runtime

import "testing"

// The substrate runtime has no exec/attach/TTY primitive and
// must opt out. This method is the sole source of the substrate refusal on
// every path (broker pre-upgrade gate, control-channel gate, /info,
// heartbeat, registration) — no caller may gate on an agentRuntime string
// literal instead; flipping it to true would silently re-enable a
// post-upgrade failure for substrate agents.
func TestHasAttachSupport_SubstrateRuntimeOptsOut(t *testing.T) {
	if HasAttachSupport(&SubstrateRuntime{}) {
		t.Error("HasAttachSupport(&SubstrateRuntime{}) = true, want false: substrate has no attach primitive")
	}
}
