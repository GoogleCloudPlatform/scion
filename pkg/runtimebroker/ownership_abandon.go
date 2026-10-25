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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// Releasing the reservation of a create that failed before its agent
// existed (ptone/scion#5768). A create reserves its slug first
// (BeginRun); if it fails before any runtime object exists, the run must
// not hold the slug. Three paths release it, each only when the agent's
// files are proven absent:
//   - AbandonCreateRun, when the create request itself fails before
//     Manager.Start (the run never recorded an object);
//   - RetireEndedCreate, when a failed create's cleanup finished its run
//     and removed its files;
//   - RetireAbandonedRecords, at start-up, for records an older build
//     left behind.
// A record that never recorded an object is deleted, not tombstoned, so
// the Hub's same-request resend (same agent ID and run ID) can reserve
// the slug again.

// beganRun is what BeginRun did: whether it created the record, and the
// record revision it wrote.
type beganRun struct {
	created  bool
	revision int64
}

// slugHeldError is a slug held by another agent of this instance. It
// unwraps to ErrOwnershipSlugHeld.
type slugHeldError struct {
	projectID, slug, holder string
}

func (e *slugHeldError) Error() string {
	return fmt.Sprintf("%v: slug %q in project %q is held by agent %s", ErrOwnershipSlugHeld, e.slug, e.projectID, e.holder)
}

func (e *slugHeldError) Unwrap() error { return ErrOwnershipSlugHeld }

// abandonOutcome is what AbandonCreateRun did.
type abandonOutcome int

const (
	abandonNone          abandonOutcome = iota // nothing changed
	abandonRunRemoved                          // the run entry was removed; record and slug kept
	abandonRecordDeleted                       // the record was deleted and the slug released
)

// knownResourceKinds are the object kinds this build records. A record
// with another kind (from a newer build) is never retired.
var knownResourceKinds = map[string]bool{
	api.ResourceKindSecret: true, api.ResourceKindSecretProviderClass: true, api.ResourceKindPod: true,
	api.ResourceKindContainer: true, api.ResourceKindSandbox: true, api.ResourceKindCloudRunInstance: true,
}

// AbandonCreateRun undoes the reservation of a failed create's run that
// recorded no object. Under the record's key lock it first calls release
// (when set), which drops the caller's in-flight hold on the run and
// reports whether the caller was the run's only holder; nothing changes
// unless it was. The run must be exactly provisioning with no resources,
// and the record active.
//
// The record is deleted, and the slug index entry removed if it names this
// agent, only when this run is the record's only run, the record revision
// is still expectRevision, and the files are proven absent: wroteNothing
// (this request created the record and wrote no agent file) or files is
// agent.FilesAbsent. slugMu is held across both removals, index first, so
// no other agent can reserve the slug while this record exists, and a crash
// between them leaves only an unindexed record (repaired or retired at the
// next start-up). Otherwise only the run entry is removed: the record and
// slug stay, a resend of the same run can add it again, and the Hub's
// delete can retire the record once no run is live.
//
// The caller holds the host's slug reservation lock (reserveMu), so no
// other instance of the host reserves the slug between the removals.
func (s *OwnershipStore) AbandonCreateRun(projectID, agentID, runID string, wroteNothing bool, expectRevision int64, files agent.FilesPresence, release func() bool) (abandonOutcome, error) {
	l := s.keyLock(projectID, agentID)
	l.Lock()
	defer l.Unlock()
	sole := true
	if release != nil {
		sole = release()
	}
	if !sole || s.agentConflict(projectID, agentID) != nil {
		return abandonNone, nil
	}
	rec, ok, err := s.read(projectID, agentID)
	if err != nil || !ok {
		return abandonNone, err
	}
	if rec.State != OwnershipStateActive {
		return abandonNone, nil
	}
	run := rec.Run(runID)
	if run == nil || run.State != OwnershipStateProvisioning || len(run.Resources) != 0 {
		return abandonNone, nil
	}
	if len(rec.Runs) == 1 && rec.Revision == expectRevision && (wroteNothing || files == agent.FilesAbsent) {
		if err := s.removeRecordAndSlug(rec, false); err != nil {
			return abandonNone, err
		}
		return abandonRecordDeleted, nil
	}
	runs := rec.Runs[:0:0]
	for _, r := range rec.Runs {
		if r.RunID != runID {
			runs = append(runs, r)
		}
	}
	rec.Runs = runs
	rec.Revision++
	rec.UpdatedAt = s.now().UTC()
	p, _ := s.recordPath(projectID, agentID)
	if err := writeOwnershipFile(p, mustJSON(rec)); err != nil {
		return abandonNone, err
	}
	return abandonRunRemoved, nil
}

// RetireEndedCreate retires the record of a failed create whose cleanup
// ended every run and removed the agent's files: files must be
// agent.FilesAbsent, established after that cleanup. Under the key lock it
// first calls release (when set; see AbandonCreateRun). It does nothing
// unless the caller was the only holder, the record is active, and every
// run is exactly deleted with every object absent. A record that never
// recorded an object is deleted; otherwise it becomes a tombstone (the
// delete's own result). The slug index entry is removed only if it names
// this agent, so an agent that reserved the slug after an earlier release
// keeps it. The caller holds reserveMu.
func (s *OwnershipStore) RetireEndedCreate(projectID, agentID string, files agent.FilesPresence, release func() bool) (bool, error) {
	l := s.keyLock(projectID, agentID)
	l.Lock()
	defer l.Unlock()
	sole := true
	if release != nil {
		sole = release()
	}
	if !sole || files != agent.FilesAbsent || s.agentConflict(projectID, agentID) != nil {
		return false, nil
	}
	rec, ok, err := s.read(projectID, agentID)
	if err != nil || !ok {
		return false, err
	}
	if rec.State != OwnershipStateActive || len(rec.Runs) == 0 {
		return false, nil
	}
	for i := range rec.Runs {
		if !endedRun(&rec.Runs[i]) {
			return false, nil
		}
	}
	if err := s.removeRecordAndSlug(rec, recordedAnObject(rec)); err != nil {
		return false, err
	}
	return true, nil
}

// RetireAbandonedRecords is the start-up pass for records left by creates
// that failed before their agent existed (ptone/scion#5768), including
// those of older builds. A record is a candidate when it is active, not
// conflicting, and each of its runs is exactly provisioning with no
// recorded object or exactly deleted with every object absent (a record
// with no run qualifies). proof establishes its files; only
// agent.FilesAbsent retires it (the same commit as RetireEndedCreate,
// after re-reading the record under its key lock and finding the same
// revision); anything else keeps it, reported through warn. It runs before
// RepairSlugIndex and before the instance serves, with nothing in flight.
func (s *OwnershipStore) RetireAbandonedRecords(proof func(OwnershipRecord) agent.FilesPresence, warn func(msg string, args ...any)) (int, error) {
	recs, err := s.List()
	if err != nil {
		return 0, err
	}
	retired := 0
	for _, r := range recs {
		if r.State != OwnershipStateActive || !abandonedShape(&r) ||
			s.agentConflict(r.ProjectID, r.AgentID) != nil || s.conflicting[OwnershipSlugKey(r.ProjectID, r.AgentSlug)] {
			continue
		}
		files := agent.FilesUnknown
		if proof != nil {
			files = proof(r)
		}
		if files != agent.FilesAbsent {
			if warn != nil {
				warn("Ownership record of an agent that never started kept: its files are not proven absent",
					"agent_id", r.AgentID, "project_id", r.ProjectID, "slug", r.AgentSlug, "files", files.String())
			}
			continue
		}
		done, err := s.retireIfUnchanged(r)
		if err != nil {
			return retired, err
		}
		if done {
			retired++
		}
	}
	return retired, nil
}

func (s *OwnershipStore) retireIfUnchanged(r OwnershipRecord) (bool, error) {
	l := s.keyLock(r.ProjectID, r.AgentID)
	l.Lock()
	defer l.Unlock()
	cur, ok, err := s.read(r.ProjectID, r.AgentID)
	if err != nil || !ok {
		return false, err
	}
	if cur.Revision != r.Revision || cur.State != OwnershipStateActive || !abandonedShape(cur) {
		return false, nil
	}
	if err := s.removeRecordAndSlug(cur, recordedAnObject(cur)); err != nil {
		return false, err
	}
	return true, nil
}

// abandonedShape reports whether every run of the record is exactly
// provisioning with no recorded object, or exactly deleted with every
// object absent and of a known kind.
func abandonedShape(r *OwnershipRecord) bool {
	for i := range r.Runs {
		run := &r.Runs[i]
		switch {
		case run.State == OwnershipStateProvisioning && len(run.Resources) == 0:
		case endedRun(run):
		default:
			return false
		}
	}
	return true
}

// endedRun reports whether the run is exactly deleted with every object
// absent and of a known kind.
func endedRun(run *OwnedRun) bool {
	if run.State != OwnershipStateDeleted {
		return false
	}
	for _, res := range run.Resources {
		if res.State != OwnedResourceAbsent || !knownResourceKinds[res.Kind] {
			return false
		}
	}
	return true
}

func recordedAnObject(r *OwnershipRecord) bool {
	for _, run := range r.Runs {
		if len(run.Resources) > 0 {
			return true
		}
	}
	return false
}

// removeRecordAndSlug releases rec's slug and then removes the record
// (tombstone false) or marks it deleted (tombstone true), all under
// slugMu. The caller holds rec's key lock. The index entry is removed only
// if it names rec's agent.
func (s *OwnershipStore) removeRecordAndSlug(rec *OwnershipRecord, tombstone bool) error {
	s.slugMu.Lock()
	defer s.slugMu.Unlock()
	holder, err := s.slugHolder(rec.ProjectID, rec.AgentSlug)
	if err != nil {
		return err
	}
	if holder == rec.AgentID {
		sp, _ := s.slugPath(rec.ProjectID, rec.AgentSlug)
		if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		syncDirBestEffort(filepath.Dir(sp))
	}
	if testHookBetweenSlugAndRecordRemoval != nil {
		testHookBetweenSlugAndRecordRemoval()
	}
	p, _ := s.recordPath(rec.ProjectID, rec.AgentID)
	if tombstone {
		rec.State = OwnershipStateDeleted
		rec.FilesRemoved = true
		rec.Revision++
		rec.UpdatedAt = s.now().UTC()
		return writeOwnershipFile(p, mustJSON(rec))
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syncDirBestEffort(filepath.Dir(p))
	return nil
}

// testHookBetweenSlugAndRecordRemoval, set only by tests, runs after the
// slug index entry is removed and before the record is removed or
// tombstoned, with every lock still held.
var testHookBetweenSlugAndRecordRemoval func()
