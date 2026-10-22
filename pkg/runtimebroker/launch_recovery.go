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

package runtimebroker

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Boot recovery (design wave 3 §1 C-b..C-d, ptone/scion#4668): a broker
// process reports and cleans up the async launches that an earlier process
// of the same broker left in the launch journal.
//
// Recovery never registers a recovered launch in launchRegistry: the launch
// is dead, so it must not be reported as a start in flight, cannot be
// superseded, and can never cancel or delay a newer start of the same name.
// Only its name-addressed cleanup runs inside a short registry slot taken
// with TryBegin, and only when nothing holds the name.

// recoveryMinSendTTL is the shortest time recovery spends sending a
// recovered launch's terminal. A record past its own report TTL (deadline +
// 10 min) still gets one attempt this long (design wave 3 §1 C-c, L5).
const recoveryMinSendTTL = 30 * time.Second

// launchRestartedMessage is the report text of a launch a restarted broker
// found unfinished in its journal.
const launchRestartedMessage = "the runtime broker restarted during the launch"

// recoveryTerminalTTL is how long recovery retries a recovered launch's
// terminal: until the launch's report TTL, and never less than
// recoveryMinSendTTL.
func recoveryTerminalTTL(deadline, now time.Time) time.Duration {
	ttl := deadline.Add(scionrt.AsyncLaunchReportTTL).Sub(now)
	if ttl < recoveryMinSendTTL {
		ttl = recoveryMinSendTTL
	}
	return ttl
}

// recoverLaunches starts recovery of every other instance's journal whose
// lock can be taken (that process is gone). It returns at once; recovery
// runs in the background, bounded by the drain's recovery context.
func (s *Server) recoverLaunches() {
	j := s.launchJournal
	if j == nil {
		return
	}
	entries, err := os.ReadDir(j.root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.agentLifecycleLog.Warn("Launch recovery: cannot read the launch journal", "dir", j.root, "error", err)
		}
		return
	}
	ids := map[string]bool{}
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".lock")
		if (e.IsDir() || id != e.Name()) && journalNameRE.MatchString(id) && id != j.instanceID {
			ids[id] = true
		}
	}
	for id := range ids {
		s.launchDrain.recoveryWG.Add(1)
		go func(id string) {
			defer s.launchDrain.recoveryWG.Done()
			s.recoverInstance(id)
		}(id)
	}
}

// recoverInstance recovers one dead instance's records under its lock, then
// removes its directory and lock file once every record is gone.
func (s *Server) recoverInstance(id string) {
	j := s.launchJournal
	lock, err := lockJournalInstance(j.root, id, false)
	if err != nil {
		if !errors.Is(err, errJournalLocked) {
			s.agentLifecycleLog.Warn("Launch recovery: cannot lock a journal instance", "instance_id", id, "error", err)
		}
		return
	}
	defer unlockJournalInstance(lock)

	dir := filepath.Join(j.root, id)
	files, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		s.agentLifecycleLog.Warn("Launch recovery: cannot read a journal instance", "instance_id", id, "error", err)
		return
	}
	var wg sync.WaitGroup
	for _, f := range files {
		name := f.Name()
		path := filepath.Join(dir, name)
		switch {
		case strings.HasSuffix(name, ".tmp"):
			// A write the dead process never renamed.
			_ = os.Remove(path)
		case strings.HasSuffix(name, ".json"):
			jr, err := readJournalRecord(path)
			if err != nil {
				s.agentLifecycleLog.Warn("Launch recovery: unreadable journal record left in place", "path", path, "error", err)
				continue
			}
			if jr.InstanceID != id || jr.LaunchID+".json" != name {
				s.agentLifecycleLog.Warn("Launch recovery: journal record does not match its location; left in place", "path", path)
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.recoverLaunch(path, jr)
			}()
		}
	}
	wg.Wait()
	if err := os.Remove(dir); err == nil || errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(filepath.Join(j.root, id+".lock"))
	}
}

// recoverLaunch sends one recovered launch's terminal under the instance ID
// it was journalled with (design wave 3 §1 C-b: the hub fences reports by
// that ID), cleans up on a confirming answer with the existing predicates,
// and removes the record (or keeps it as cleanup_pending if the clean-up
// fails, ptone/scion#5177). With no definitive answer by its TTL, the
// record is kept for the next boot to resend, up to a bound, after which it
// is removed with no cleanup and `scion delete` removes what is left
// (ptone/scion#5186). If the broker shuts down first, the record stays for
// the next boot.
func (s *Server) recoverLaunch(path string, jr *journalRecord) {
	ctx := s.recoveryContext()
	if ctx.Err() != nil {
		return
	}
	rec := newLaunchRecord(jr.LaunchID, jr.AgentID, jr.Kind, jr.HubName, jr.Deadline, func() {})
	rec.RunID = jr.RunID
	rec.Handles = append([]api.ResourceHandle(nil), jr.Handles...)
	if jr.OwnerHub != "" {
		rec.SetOwnerHub(jr.OwnerHub)
	}
	if o := jr.OwnedRun; o != nil {
		rec.ownedRun = ownedRunKey{projectID: o.ProjectID, agentID: o.AgentID, runID: o.RunID}
	}
	sender := newLaunchSender(s, rec, jr.AgentID, jr.InstanceID, 0)
	sender.seq = jr.Seq

	ttl := recoveryTerminalTTL(jr.Deadline, time.Now())
	if s.launchRecoveryTTL > 0 {
		ttl = s.launchRecoveryTTL
	}
	sendCtx, cancel := context.WithTimeout(ctx, ttl)
	defer cancel()
	if jr.State == journalCleanupPending.String() {
		// The hub already confirmed the launch is over and asked for
		// clean-up; only the clean-up is left (ptone/scion#5177).
		s.finishRecoveredCleanup(ctx, path, jr, s.recoveryCleanup(ctx, jr, rec))
		return
	}
	// A decided terminal is resent unchanged, so a succeeded is never
	// turned into a failure; an unfinished launch reports broker_restarted.
	var t journalTerminal
	switch {
	case jr.State == journalTerminalPending.String() && jr.Terminal != nil:
		t = *jr.Terminal
	case jr.State == journalLaunching.String():
		t = journalTerminal{Step: stepOr(jr.LastStep, api.LaunchStepLaunching), ErrorCode: api.LaunchCodeBrokerRestarted, Message: launchRestartedMessage}
	default:
		s.agentLifecycleLog.Warn("Launch recovery: journal record has an unknown state; removed", "launch_id", jr.LaunchID, "state", jr.State)
		_ = os.Remove(path)
		return
	}
	result, err := sender.SendTerminal(sendCtx, t.Succeeded, t.Step, t.ErrorCode, t.Message, t.Cause, t.AgentInfo)
	if err != nil {
		if ctx.Err() != nil {
			// The broker is shutting down: the next boot tries again.
			return
		}
		// No definitive answer: keep the record for the next boot to
		// resend, up to the bound (ptone/scion#5186).
		s.keepRecoveredReport(path, jr, t)
		return
	}
	// The existing predicates decide cleanup, unchanged.
	cleanup := shouldCleanupAfterFailureReport(result)
	if t.Succeeded {
		cleanup = shouldCleanupAfterSucceededReport(result)
	}
	s.agentLifecycleLog.Info("Launch recovery: reported a launch an earlier broker process left unfinished",
		"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "instance_id", jr.InstanceID,
		"state", jr.State, "succeeded", t.Succeeded, "error_code", t.ErrorCode, "cleanup", cleanup)
	var cleanupErr error
	if cleanup {
		cleanupErr = s.recoveryCleanup(ctx, jr, rec)
	}
	s.finishRecoveredCleanup(ctx, path, jr, cleanupErr)
}

// recoveryCleanup removes a recovered launch's resources on a confirming
// answer: its journalled handles (each delete preconditioned on the UID
// recorded at create time, so a newer launch's objects are untouched), then
// its name-addressed leftovers inside a registry slot.
func (s *Server) recoveryCleanup(ctx context.Context, jr *journalRecord, rec *launchRecord) error {
	mgr := s.recoveryManager(jr)
	if mgr == nil && jr.RuntimeIdentity == "" {
		s.agentLifecycleLog.Warn("Launch recovery: the launch's runtime identity was not recorded; its resources are left for `scion delete`",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "runtime", jr.RuntimeName)
		return nil
	}
	if mgr == nil {
		s.agentLifecycleLog.Warn("Launch recovery: the launch's runtime is no longer configured; its resources are left for `scion delete`",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "runtime", jr.RuntimeName, "runtime_identity", jr.RuntimeIdentity)
		return failCleanup(rec, cleanupFailRuntimeUnavailable)
	}
	if jr.State == journalCleanupPending.String() && jr.CleanupScope == cleanupScopeResources {
		// A local stop or delete owned the files: retry only the
		// recorded resources, as the live clean-up did.
		return s.cleanupLaunchResources(mgr, rec)
	}
	return errors.Join(
		s.cleanupLaunchResources(mgr, rec),
		s.recoveryNameCleanup(ctx, mgr, jr, rec),
	)
}

// recoveryManager resolves the manager of the exact runtime a launch ran on,
// by the identity journalled at admission (auxiliaryRuntimeIdentity: for
// Kubernetes the context and namespace too, so a broker with two clusters
// never recovers a launch against the other one). It looks at the default
// runtime, then the auxiliary runtimes (keyed by that identity), then
// resolves the launch's profile and keeps the result only if its identity
// matches. Nil when none matches, or when the launch's identity is unknown:
// recovery then cleans up nothing rather than guess.
func (s *Server) recoveryManager(jr *journalRecord) agent.Manager {
	defMgr, defRT, _ := s.defaultPair()
	if s.isFlat() {
		// A flat instance serves exactly one runtime target.
		return defMgr
	}
	id := jr.RuntimeIdentity
	if id == "" {
		return nil
	}
	if defRT != nil && auxiliaryRuntimeIdentity(defRT) == id {
		return defMgr
	}
	s.auxiliaryRuntimesMu.RLock()
	aux, ok := s.auxiliaryRuntimes[id]
	s.auxiliaryRuntimesMu.RUnlock()
	if ok && aux.Manager != nil {
		return aux.Manager
	}
	if jr.Profile == "" {
		return nil
	}
	res, err := s.resolveManagerForOptsStrict(api.StartOptions{Name: jr.AgentName, ProjectPath: jr.ProjectPath, Profile: jr.Profile}, profileLenient, slog.LevelWarn)
	mgr := res.Manager
	if err != nil || mgr == nil || s.launchRuntimeIdentity(mgr) != id {
		return nil
	}
	return mgr
}

// launchRuntimeIdentity returns the identity of the runtime behind mgr
// (auxiliaryRuntimeIdentity), for the launch journal: the runtime of an
// agent.AgentManager, else the default or auxiliary runtime mgr is the
// manager of. "" if unknown.
func (s *Server) launchRuntimeIdentity(mgr agent.Manager) string {
	if mgr == nil {
		return ""
	}
	if am, ok := mgr.(*agent.AgentManager); ok {
		if am.Runtime == nil {
			return ""
		}
		return auxiliaryRuntimeIdentity(am.Runtime)
	}
	if !reflect.TypeOf(mgr).Comparable() {
		return ""
	}
	if defMgr, defRT, _ := s.defaultPair(); defRT != nil && defMgr != nil && reflect.TypeOf(defMgr).Comparable() && defMgr == mgr {
		return auxiliaryRuntimeIdentity(defRT)
	}
	s.auxiliaryRuntimesMu.RLock()
	defer s.auxiliaryRuntimesMu.RUnlock()
	for id, aux := range s.auxiliaryRuntimes {
		if aux.Manager != nil && reflect.TypeOf(aux.Manager).Comparable() && aux.Manager == mgr {
			return id
		}
	}
	return ""
}

// recoveryNameCleanup runs a recovered launch's name-addressed cleanup: the
// delete of the runtime entry labelled with its run (deleteRecoveredRun),
// then its marker- and owner-checked agent files (create launches only).
// Both address the agent by name, so they run only while nothing else holds
// it: no tracked start of the name, and a registry slot taken with TryBegin,
// which never cancels anyone. A start that registers during the slot
// cancels it, and recovery checks that before each call (design wave 3 §1
// C-c step 5).
func (s *Server) recoveryNameCleanup(ctx context.Context, mgr agent.Manager, jr *journalRecord, rec *launchRecord) error {
	key := jr.key()
	slotCtx, cancel := context.WithTimeout(ctx, scionrt.AsyncLaunchCleanupBudget)
	defer cancel()
	slot := newLaunchRecord(jr.LaunchID, jr.AgentID, jr.Kind, "", jr.Deadline, cancel)
	slot.RunID = jr.RunID
	if s.startsInFlight.anyFor(key) || !s.launchRegistry.TryBegin(key, slot) {
		s.agentLifecycleLog.Info("Launch recovery: the agent name is in use by a newer start; leftover resources are removed by `scion delete`",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID)
		return nil
	}
	defer s.launchRegistry.Finish(key, slot)
	if s.startsInFlight.anyFor(key) {
		s.agentLifecycleLog.Info("Launch recovery: the agent name is in use by a newer start; leftover resources are removed by `scion delete`",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID)
		return nil
	}
	var runErr error
	if jr.RunID != "" && slotCtx.Err() == nil {
		runErr = s.deleteRecoveredRun(slotCtx, mgr, jr)
	}
	if slotCtx.Err() != nil {
		s.agentLifecycleLog.Info("Launch recovery: name-addressed cleanup stopped: a newer start took the name, or the broker is shutting down",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID)
		return runErr
	}
	opts := api.StartOptions{
		Name:            jr.AgentName,
		ProjectPath:     jr.ProjectPath,
		HubProjectID:    jr.HubProjectID,
		SharedWorkspace: jr.SharedWorkspace,
		RunID:           jr.RunID,
	}
	return errors.Join(runErr, s.cleanupLaunchFiles(rec, opts, jr.SharedWorkspace, jr.Slug))
}

// deleteRecoveredRun deletes the runtime entry labelled with the recovered
// launch's run, for an object created after the launch's last journal write
// (design wave 3 §1 C-d). Only an entry labelled with that run exactly is
// deleted, never an unlabelled legacy entry or another run's; not found
// counts as done.
func (s *Server) deleteRecoveredRun(ctx context.Context, mgr agent.Manager, jr *journalRecord) error {
	infos, err := mgr.List(ctx, map[string]string{"scion.name": api.Slugify(jr.AgentName)})
	if err != nil {
		s.agentLifecycleLog.Warn("Launch recovery: cannot list the agent's runtime entries; run-scoped cleanup skipped",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "error", err)
		return failCleanup(nil, cleanupFailRunList)
	}
	target, found, err := agent.SelectExactRunTarget(infos, jr.AgentName, jr.ProjectPath, jr.RunID)
	if err != nil {
		s.agentLifecycleLog.Warn("Launch recovery: run-scoped cleanup skipped", "agent_id", jr.AgentID, "launch_id", jr.LaunchID, "error", err)
		return nil
	}
	if !found {
		return nil
	}
	ref := scionrt.RunRef{ID: scionrt.AgentOperationID(target), RunID: target.RunID}
	if _, err := mgr.DeleteTarget(ctx, jr.AgentName, ref, false, jr.ProjectPath, false); err != nil {
		s.agentLifecycleLog.Warn("Launch recovery: failed to delete the launch's runtime entry",
			"agent_id", jr.AgentID, "launch_id", jr.LaunchID, "run_id", jr.RunID, "error", err)
		return failCleanup(nil, cleanupFailRunDelete)
	}
	return nil
}
