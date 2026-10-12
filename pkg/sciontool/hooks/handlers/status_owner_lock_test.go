//go:build unix

/*
Copyright 2026 The Scion Authors.
*/

package handlers

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatusHandlerOwnerListAndStatusWritersLoseNothing runs owner-list
// updates (a sub-agent question adding its owner, then SubagentStop
// releasing it) in one handler against detail.message writes in another,
// as two hook processes would. With every owner-list change inside the
// locked update, neither side's change is lost: the owner list only grows
// in the add phase and only shrinks in the release phase, and the message
// never goes back below the last one written. The 10s test lock wait keeps
// both writers out of the unlocked fallback.
func TestStatusHandlerOwnerListAndStatusWritersLoseNothing(t *testing.T) {
	t.Setenv("SCION_TEST_AGENTINFO_LOCK_WAIT", "10s")
	path := filepath.Join(t.TempDir(), "agent-info.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"template":"tpl","phase":"running"}`), 0o644))
	owners := &StatusHandler{StatusPath: path}
	status := &StatusHandler{StatusPath: path}

	const nOwners = 60 // under maxNotificationWaitAgents
	const nMsgs = 200
	require.Less(t, nOwners, maxNotificationWaitAgents)

	var added, released, msgs atomic.Int64
	msgs.Store(-1)
	var lost atomic.Bool
	check := func(who string) {
		wantAdded, wantReleased, wantMsg := added.Load(), released.Load(), msgs.Load()
		info := readInfo(t, path)
		n := int64(len(notificationWaitOwners(info)))
		detail, _ := info["detail"].(map[string]any)
		gotMsg := readCounter(detail["message"], "m")
		// Each bound only uses counters loaded before the read, and a
		// later write only moves the count further the same way. Once all
		// owners are added, a release may already be on disk before its
		// counter moves, so only the upper bound applies from then on.
		var ownersOK bool
		switch {
		case wantReleased > 0 || wantAdded == nOwners:
			ownersOK = n <= nOwners-wantReleased
		default:
			ownersOK = n >= wantAdded
		}
		if (!ownersOK || gotMsg < wantMsg || info["template"] != "tpl") && lost.CompareAndSwap(false, true) {
			t.Errorf("%s saw a lost update: %d owners (added %d, released %d), message m-%d (finished m-%d), template %v",
				who, n, wantAdded, wantReleased, gotMsg, wantMsg, info["template"])
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < nOwners && !lost.Load(); i++ {
			if err := owners.setWaitingFromNotificationFor(fmt.Sprintf("sa-%d", i)); err != nil {
				t.Errorf("add: %v", err)
				return
			}
			added.Add(1)
			check("add")
		}
		for i := 0; i < nOwners && !lost.Load(); i++ {
			if err := owners.releaseNotificationWaitOwner(fmt.Sprintf("sa-%d", i)); err != nil {
				t.Errorf("release: %v", err)
				return
			}
			released.Add(1)
			check("release")
		}
	}()
	go func() {
		defer wg.Done()
		for i := int64(0); i < nMsgs && !lost.Load(); i++ {
			if err := status.SetMessage(fmt.Sprintf("m-%d", i)); err != nil {
				t.Errorf("status: %v", err)
				return
			}
			msgs.Store(i)
			check("status")
		}
	}()
	wg.Wait()
	check("final")

	info := readInfo(t, path)
	assert.Empty(t, notificationWaitOwners(info), "every owner was released")
	assert.Equal(t, "waiting_for_input", info["activity"])
	assert.Equal(t, activitySourceNotification, info[activitySourceKey])
	detail, _ := info["detail"].(map[string]any)
	assert.Equal(t, fmt.Sprintf("m-%d", nMsgs-1), detail["message"])
}
