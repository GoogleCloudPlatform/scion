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

// The substrate runtime does not call the async launch hooks, so it must
// opt out of async launch; a runtime without the method keeps the default.
func TestHasAsyncLaunchSupport(t *testing.T) {
	if HasAsyncLaunchSupport(&SubstrateRuntime{}) {
		t.Error("HasAsyncLaunchSupport(&SubstrateRuntime{}) = true, want false: substrate does not call Checkpoint/OnResourceCreated")
	}
	if !HasAsyncLaunchSupport(&MockRuntime{}) {
		t.Error("HasAsyncLaunchSupport(&MockRuntime{}) = false, want true: a runtime without the method is supported")
	}
}
