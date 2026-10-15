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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	scionrt "github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Durable ownership records of a flat Runtime Broker instance
// (ptone/scion#3274). Each instance keeps them in its
// own broker state root, never in an agent home. They are the authority for
// which agents (including file-only agents with no runtime object) the
// instance may operate on; the reserved runtime label
// (api.LabelRuntimeBrokerID) is the authority for runtime objects. A record
// is the durable mirror of the launch journal (launchRecord handles), not a
// second state machine.
//
// Key: (projectID, agentID). The agent slug is a locator, indexed with a
// uniqueness check so a different agent reusing a slug never inherits the
// holder's record. Writes are serialized per key, carry a monotonically
// increasing revision, and never move a state backwards. Deleted records and
// runs stay as tombstones (no garbage collection in P2); a handle append
// needs an existing live run, so a late callback can never recreate or
// revive a record or run. Releasing a deleted agent's slug (ReleaseSlug)
// lets a new agent ID use it while the tombstone stays.

// Record and run states, in their only allowed order.
const (
	OwnershipStateProvisioning = "provisioning" // run only
	OwnershipStateActive       = "active"       // record only
	OwnershipStateCreated      = "created"      // run only
	OwnershipStateDeleting     = "deleting"
	OwnershipStateDeleted      = "deleted"
)

// Resource states.
const (
	OwnedResourceRecorded = "recorded"
	OwnedResourceAbsent   = "absent"
)

var runStateOrder = map[string]int{OwnershipStateProvisioning: 0, OwnershipStateCreated: 1, OwnershipStateDeleting: 2, OwnershipStateDeleted: 3}
var recordStateOrder = map[string]int{OwnershipStateActive: 0, OwnershipStateDeleting: 1, OwnershipStateDeleted: 2}

const ownershipSchemaVersion = 1

// OwnedResource is one runtime object a run created (a launch resource
// handle). UID is the identity a delete is fenced on.
type OwnedResource struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	State     string `json:"state"`
}

// OwnedRun is one run of an owned agent.
type OwnedRun struct {
	RunID     string          `json:"runId"`
	State     string          `json:"state"`
	Resources []OwnedResource `json:"resources,omitempty"`
}

// OwnershipRecord is the durable ownership record of one agent.
type OwnershipRecord struct {
	SchemaVersion   int        `json:"schemaVersion"`
	Revision        int64      `json:"revision"`
	RuntimeBrokerID string     `json:"runtimeBrokerId"`
	ProjectID       string     `json:"projectId"`
	AgentID         string     `json:"agentId"`
	AgentSlug       string     `json:"agentSlug"`
	State           string     `json:"state"`
	Runs            []OwnedRun `json:"runs"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	// FilesRemoved is set once a whole-agent delete removed the agent's
	// files (the runtime delete and file removal succeeded); only then may
	// the start-up reconciliation finish the record and release the slug.
	FilesRemoved bool `json:"filesRemoved,omitempty"`
}

// Run returns the run with this ID, or nil.
func (r *OwnershipRecord) Run(runID string) *OwnedRun {
	for i := range r.Runs {
		if r.Runs[i].RunID == runID {
			return &r.Runs[i]
		}
	}
	return nil
}

// OwnsUIDAny reports whether any run recorded the object with this UID and
// its absence has not been established, whatever the record's state.
func (r *OwnershipRecord) OwnsUIDAny(uid string) bool {
	for _, run := range r.Runs {
		for _, res := range run.Resources {
			if res.UID == uid && res.State == OwnedResourceRecorded {
				return true
			}
		}
	}
	return false
}

// OwnsUID reports whether a run of an active record recorded the object with
// this UID (and its absence has not been established).
func (r *OwnershipRecord) OwnsUID(uid string) bool {
	if uid == "" || r.State != OwnershipStateActive {
		return false
	}
	for _, run := range r.Runs {
		for _, res := range run.Resources {
			if res.UID == uid && res.State == OwnedResourceRecorded {
				return true
			}
		}
	}
	return false
}

// Errors returned by the store.
var (
	ErrOwnershipKeyInvalid  = errors.New("invalid ownership key")
	ErrOwnershipNotRecorded = errors.New("not recorded by this Runtime Broker instance")
	ErrOwnershipSlugHeld    = errors.New("agent slug is held by another agent of this Runtime Broker instance")
	ErrOwnershipStateOrder  = errors.New("ownership state cannot move backwards")
	ErrOwnershipUnreadable  = errors.New("ownership record unreadable")
	ErrOwnershipConflict    = errors.New("ownership is claimed by more than one configured Runtime Broker instance")
	// ErrOwnershipSlugPending is a slug still reserved by an agent whose
	// delete has not yet confirmed that all its objects are gone.
	ErrOwnershipSlugPending = errors.New("agent slug is still reserved by an earlier delete whose cleanup is unconfirmed")
)

// OwnershipAgentKey and OwnershipSlugKey are the ownership keys compared
// across a host's instances (LiveKeys, SetConflicting).
func OwnershipAgentKey(projectID, agentID string) string { return "agent:" + projectID + "/" + agentID }

// OwnershipSlugKey is the ownership key of a slug in a project.
func OwnershipSlugKey(projectID, slug string) string { return "slug:" + projectID + "/" + slug }

// OwnershipStore reads and writes one instance's ownership records under
// <stateDir>/ownership/<projectId>/<agentId>.json, with the slug index under
// <stateDir>/ownership/<projectId>/by-slug/<agentSlug>.
type OwnershipStore struct {
	dir      string
	brokerID string
	now      func() time.Time

	locksMu sync.Mutex
	locks   map[string]*sync.Mutex
	// slugMu serializes slug index changes (one mutex for the whole store,
	// every project).
	slugMu sync.Mutex
	// conflicting are keys another configured instance claims too; every
	// operation on them is refused (set once, before the store is used).
	conflicting map[string]bool
	// addResourceFault, set only by tests, fails AddResource for a handle
	// before anything is written.
	addResourceFault func(api.ResourceHandle) error
}

// SetConflicting marks ownership keys (OwnershipAgentKey,
// OwnershipSlugKey) that another configured instance also claims. Reads and
// writes of those keys fail with ErrOwnershipConflict. Call it once, before
// the store is used.
func (s *OwnershipStore) SetConflicting(keys map[string]bool) {
	s.conflicting = make(map[string]bool, len(keys))
	for k, v := range keys {
		if v {
			s.conflicting[k] = true
		}
	}
}

// ConflictingLabels reports whether a runtime object's labels name an agent
// or slug whose ownership is conflicting.
func (s *OwnershipStore) ConflictingLabels(labels map[string]string) bool {
	if len(s.conflicting) == 0 {
		return false
	}
	p := labels["scion.project_id"]
	return s.conflicting[OwnershipAgentKey(p, labels["agent_id"])] || s.conflicting[OwnershipSlugKey(p, labels["scion.name"])]
}

func (s *OwnershipStore) agentConflict(projectID, agentID string) error {
	if s.conflicting[OwnershipAgentKey(projectID, agentID)] {
		return fmt.Errorf("%w: agent %q in project %q", ErrOwnershipConflict, agentID, projectID)
	}
	return nil
}

// LiveKeys returns the agent and slug keys of every live (not deleted)
// record, for comparison with the host's other instances. An unreadable
// record is an error.
func (s *OwnershipStore) LiveKeys() ([]string, error) {
	recs, err := s.List()
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, r := range recs {
		if r.State == OwnershipStateDeleted {
			continue
		}
		keys = append(keys, OwnershipAgentKey(r.ProjectID, r.AgentID), OwnershipSlugKey(r.ProjectID, r.AgentSlug))
	}
	return keys, nil
}

// NewOwnershipStore returns the store of the instance with this Runtime
// Broker ID whose broker state root is stateDir.
func NewOwnershipStore(stateDir, runtimeBrokerID string) *OwnershipStore {
	return &OwnershipStore{dir: filepath.Join(stateDir, "ownership"), brokerID: runtimeBrokerID, now: time.Now, locks: map[string]*sync.Mutex{}}
}

// RuntimeBrokerID is the owner this store records.
func (s *OwnershipStore) RuntimeBrokerID() string { return s.brokerID }

func (s *OwnershipStore) keyLock(projectID, agentID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	k := projectID + "\x00" + agentID
	if s.locks[k] == nil {
		s.locks[k] = &sync.Mutex{}
	}
	return s.locks[k]
}

func validOwnershipElement(v string) bool {
	return v != "" && v != "." && v != ".." && v != "by-slug" && !strings.HasPrefix(v, ".") &&
		!strings.ContainsAny(v, "/\\\x00") && filepath.Clean(v) == v
}

func (s *OwnershipStore) recordPath(projectID, agentID string) (string, error) {
	if !validOwnershipElement(projectID) || !validOwnershipElement(agentID) {
		return "", fmt.Errorf("%w: project %q, agent %q", ErrOwnershipKeyInvalid, projectID, agentID)
	}
	return filepath.Join(s.dir, projectID, agentID+".json"), nil
}

func (s *OwnershipStore) slugPath(projectID, slug string) (string, error) {
	if !validOwnershipElement(projectID) || !validOwnershipElement(slug) {
		return "", fmt.Errorf("%w: project %q, slug %q", ErrOwnershipKeyInvalid, projectID, slug)
	}
	return filepath.Join(s.dir, projectID, "by-slug", slug), nil
}

// Get returns the record for the key; ok is false when there is none. A
// record that cannot be read or does not belong to this instance and key is
// an error (unresolved), never "no record".
func (s *OwnershipStore) Get(projectID, agentID string) (*OwnershipRecord, bool, error) {
	l := s.keyLock(projectID, agentID)
	l.Lock()
	defer l.Unlock()
	return s.read(projectID, agentID)
}

func (s *OwnershipStore) read(projectID, agentID string) (*OwnershipRecord, bool, error) {
	if err := s.agentConflict(projectID, agentID); err != nil {
		return nil, false, err
	}
	return s.readFile(projectID, agentID)
}

// readFile reads the record without the conflict check (List, LiveKeys).
func (s *OwnershipStore) readFile(projectID, agentID string) (*OwnershipRecord, bool, error) {
	p, err := s.recordPath(projectID, agentID)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("%w: %s: %v", ErrOwnershipUnreadable, p, err)
	}
	var rec OwnershipRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, false, fmt.Errorf("%w: %s: %v", ErrOwnershipUnreadable, p, err)
	}
	if rec.SchemaVersion != ownershipSchemaVersion || rec.RuntimeBrokerID != s.brokerID || rec.ProjectID != projectID || rec.AgentID != agentID {
		return nil, false, fmt.Errorf("%w: %s does not match this instance or key", ErrOwnershipUnreadable, p)
	}
	return &rec, true, nil
}

// SlugHolder returns the agent ID holding slug in the project, or "". A
// conflicting slug is an error.
func (s *OwnershipStore) SlugHolder(projectID, slug string) (string, error) {
	if s.conflicting[OwnershipSlugKey(projectID, slug)] {
		return "", fmt.Errorf("%w: slug %q in project %q", ErrOwnershipConflict, slug, projectID)
	}
	return s.slugHolder(projectID, slug)
}

// SlugReservation reports the agent holding slug in the project and
// whether that agent's record is no longer live (its delete has not yet
// confirmed that all its objects are gone). A slug index entry whose record
// is missing counts as held and live. An unreadable or conflicting entry
// is an error.
func (s *OwnershipStore) SlugReservation(projectID, slug string) (holder string, pending bool, err error) {
	holder, err = s.SlugHolder(projectID, slug)
	if err != nil || holder == "" {
		return holder, false, err
	}
	rec, found, err := s.readFile(projectID, holder)
	if err != nil {
		return "", false, err
	}
	return holder, found && rec.State != OwnershipStateActive, nil
}

func (s *OwnershipStore) slugHolder(projectID, slug string) (string, error) {
	p, err := s.slugPath(projectID, slug)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrOwnershipUnreadable, p, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// mutate applies fn to the record under the key lock and writes it with the
// next revision.
func (s *OwnershipStore) mutate(projectID, agentID string, fn func(*OwnershipRecord) error) (*OwnershipRecord, error) {
	l := s.keyLock(projectID, agentID)
	l.Lock()
	defer l.Unlock()
	rec, ok, err := s.read(projectID, agentID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: agent %q in project %q", ErrOwnershipNotRecorded, agentID, projectID)
	}
	if err := fn(rec); err != nil {
		return nil, err
	}
	rec.Revision++
	rec.UpdatedAt = s.now().UTC()
	p, _ := s.recordPath(projectID, agentID)
	if err := writeOwnershipFile(p, mustJSON(rec)); err != nil {
		return nil, err
	}
	return rec, nil
}

// BeginRun records a run in the provisioning state, creating the record
// (and claiming the slug) when the agent has none. It is called before any
// agent file becomes discoverable and before the runtime creates anything.
// A slug held by a different agent ID is refused; a deleting or deleted
// record cannot take a new run.
func (s *OwnershipStore) BeginRun(projectID, agentID, slug, runID string) error {
	if runID == "" {
		return errors.New("ownership: a run ID is required")
	}
	if _, err := s.slugPath(projectID, slug); err != nil {
		return err
	}
	l := s.keyLock(projectID, agentID)
	l.Lock()
	defer l.Unlock()
	rec, ok, err := s.read(projectID, agentID)
	if err != nil {
		return err
	}
	s.slugMu.Lock()
	defer s.slugMu.Unlock()
	holder, err := s.SlugHolder(projectID, slug)
	if err != nil {
		return err
	}
	if holder != "" && holder != agentID {
		if held, found, _ := s.readFile(projectID, holder); found && held.State != OwnershipStateActive {
			return fmt.Errorf("%w: slug %q in project %q on Runtime Broker instance %s: the delete of agent %s has not yet confirmed that all its objects are gone",
				ErrOwnershipSlugPending, slug, projectID, s.brokerID, holder)
		}
		return fmt.Errorf("%w: slug %q in project %q is held by agent %s", ErrOwnershipSlugHeld, slug, projectID, holder)
	}
	if !ok {
		rec = &OwnershipRecord{SchemaVersion: ownershipSchemaVersion, RuntimeBrokerID: s.brokerID,
			ProjectID: projectID, AgentID: agentID, AgentSlug: slug, State: OwnershipStateActive}
	}
	if rec.State != OwnershipStateActive {
		return fmt.Errorf("%w: agent %q is %s", ErrOwnershipStateOrder, agentID, rec.State)
	}
	if rec.AgentSlug != slug {
		return fmt.Errorf("%w: agent %q is recorded with slug %q, not %q", ErrOwnershipKeyInvalid, agentID, rec.AgentSlug, slug)
	}
	if existing := rec.Run(runID); existing == nil {
		rec.Runs = append(rec.Runs, OwnedRun{RunID: runID, State: OwnershipStateProvisioning})
	} else if runStateOrder[existing.State] >= runStateOrder[OwnershipStateDeleting] {
		return fmt.Errorf("%w: run %q of agent %q is %s and cannot be revived", ErrOwnershipStateOrder, runID, agentID, existing.State)
	}
	rec.Revision++
	rec.UpdatedAt = s.now().UTC()
	p, _ := s.recordPath(projectID, agentID)
	if err := writeOwnershipFile(p, mustJSON(rec)); err != nil {
		return err
	}
	if holder == "" {
		sp, _ := s.slugPath(projectID, slug)
		if err := writeOwnershipFile(sp, []byte(agentID+"\n")); err != nil {
			return err
		}
	}
	return nil
}

// AddResource records an object a run created. It is refused once the run
// or record is deleting or deleted, so a late callback never adds to (or
// recreates) a record being removed.
func (s *OwnershipStore) AddResource(projectID, agentID, runID string, h api.ResourceHandle) error {
	if h.UID == "" {
		return errors.New("ownership: a resource UID is required")
	}
	if s.addResourceFault != nil {
		if err := s.addResourceFault(h); err != nil {
			return err
		}
	}
	_, err := s.mutate(projectID, agentID, func(r *OwnershipRecord) error {
		run := r.Run(runID)
		if run == nil {
			return fmt.Errorf("%w: run %q of agent %q", ErrOwnershipNotRecorded, runID, agentID)
		}
		if r.State != OwnershipStateActive || runStateOrder[run.State] >= runStateOrder[OwnershipStateDeleting] {
			return fmt.Errorf("%w: run %q of agent %q is %s", ErrOwnershipStateOrder, runID, agentID, run.State)
		}
		for _, res := range run.Resources {
			if res.UID == h.UID {
				return nil
			}
		}
		run.Resources = append(run.Resources, OwnedResource{Kind: h.Kind, Namespace: h.Namespace, Name: h.Name, UID: h.UID, State: OwnedResourceRecorded})
		return nil
	})
	return err
}

// SetRunState moves a run forward; a backward move is refused.
func (s *OwnershipStore) SetRunState(projectID, agentID, runID, state string) error {
	if _, ok := runStateOrder[state]; !ok {
		return fmt.Errorf("ownership: unknown run state %q", state)
	}
	_, err := s.mutate(projectID, agentID, func(r *OwnershipRecord) error {
		run := r.Run(runID)
		if run == nil {
			return fmt.Errorf("%w: run %q of agent %q", ErrOwnershipNotRecorded, runID, agentID)
		}
		if runStateOrder[state] < runStateOrder[run.State] {
			return fmt.Errorf("%w: run %q from %s to %s", ErrOwnershipStateOrder, runID, run.State, state)
		}
		run.State = state
		return nil
	})
	return err
}

// SetRecordState moves the record forward (active → deleting → deleted); a
// backward move is refused. Deleted records stay as tombstones until Purge.
func (s *OwnershipStore) SetRecordState(projectID, agentID, state string) error {
	if _, ok := recordStateOrder[state]; !ok {
		return fmt.Errorf("ownership: unknown record state %q", state)
	}
	_, err := s.mutate(projectID, agentID, func(r *OwnershipRecord) error {
		if recordStateOrder[state] < recordStateOrder[r.State] {
			return fmt.Errorf("%w: agent %q from %s to %s", ErrOwnershipStateOrder, agentID, r.State, state)
		}
		r.State = state
		if state == OwnershipStateDeleted {
			for i := range r.Runs {
				r.Runs[i].State = OwnershipStateDeleted
			}
		}
		return nil
	})
	return err
}

// MarkFilesRemoved records that a whole-agent delete removed the agent's
// files.
func (s *OwnershipStore) MarkFilesRemoved(projectID, agentID string) error {
	_, err := s.mutate(projectID, agentID, func(r *OwnershipRecord) error {
		r.FilesRemoved = true
		return nil
	})
	return err
}

// MarkAbsent records that the object with this UID is confirmed gone. The
// caller establishes confirmation (a successful complete scoped read or an
// exact NotFound); an inspection failure must not call this.
func (s *OwnershipStore) MarkAbsent(projectID, agentID, uid string) error {
	_, err := s.mutate(projectID, agentID, func(r *OwnershipRecord) error {
		for i := range r.Runs {
			for j := range r.Runs[i].Resources {
				if r.Runs[i].Resources[j].UID == uid {
					r.Runs[i].Resources[j].State = OwnedResourceAbsent
				}
			}
		}
		return nil
	})
	return err
}

// ReleaseSlug releases a deleted agent's slug reservation once every
// recorded object is confirmed absent. The record stays as a tombstone.
// Anything else is refused.
//
// Callers and the workspace lock:
//   - finishOwnedDelete from deleteAgentFenced holds the process-wide
//     workspace lock on the agent's files (lockAgentFiles) for the whole
//     delete, so no provisioning or other removal of those paths runs
//     before the slug is free.
//   - finishOwnedDelete from finishPendingOwnedDelete runs without that
//     lock. Its caller found neither a runtime entry nor files for the
//     agent, and while the record is deleting no new run can take the slug
//     (an active record is moved to deleting first, and only when no run
//     is live).
//   - ReconcileAbsent runs without that lock, from the instance's ownership
//     preflight, before the instance serves any request.
func (s *OwnershipStore) ReleaseSlug(projectID, agentID string) error {
	l := s.keyLock(projectID, agentID)
	l.Lock()
	defer l.Unlock()
	rec, ok, err := s.read(projectID, agentID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: agent %q in project %q", ErrOwnershipNotRecorded, agentID, projectID)
	}
	if rec.State != OwnershipStateDeleted {
		return fmt.Errorf("%w: agent %q is %s, not deleted", ErrOwnershipStateOrder, agentID, rec.State)
	}
	for _, run := range rec.Runs {
		for _, res := range run.Resources {
			if res.State != OwnedResourceAbsent {
				return fmt.Errorf("ownership: agent %q still has object %s (%s) not confirmed absent", agentID, res.UID, res.Name)
			}
		}
	}
	s.slugMu.Lock()
	defer s.slugMu.Unlock()
	holder, err := s.SlugHolder(projectID, rec.AgentSlug)
	if err != nil {
		return err
	}
	if holder != agentID {
		return nil
	}
	sp, _ := s.slugPath(projectID, rec.AgentSlug)
	if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	syncDirBestEffort(filepath.Dir(sp))
	return nil
}

// RepairSlugIndex restores record/index consistency before the instance
// serves operations: every live (not deleted) record holds its slug; an
// index entry naming a missing or deleted record is reported as a
// conflict, never silently reassigned. It returns the inconsistencies it
// could not repair (two live records claiming one slug, unreadable index or
// records), which the caller treats as unresolved ownership.
func (s *OwnershipStore) RepairSlugIndex() ([]string, error) {
	recs, err := s.List()
	if err != nil {
		return nil, err
	}
	s.slugMu.Lock()
	defer s.slugMu.Unlock()
	var problems []string
	live := map[string]string{} // project + slug -> agentID
	for _, r := range recs {
		if r.State == OwnershipStateDeleted {
			continue
		}
		k := r.ProjectID + "\x00" + r.AgentSlug
		if other, dup := live[k]; dup {
			problems = append(problems, fmt.Sprintf("slug %q in project %q is claimed by live agents %s and %s", r.AgentSlug, r.ProjectID, other, r.AgentID))
			continue
		}
		live[k] = r.AgentID
		holder, err := s.slugHolder(r.ProjectID, r.AgentSlug)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		switch holder {
		case r.AgentID:
		case "":
			sp, _ := s.slugPath(r.ProjectID, r.AgentSlug)
			if err := writeOwnershipFile(sp, []byte(r.AgentID+"\n")); err != nil {
				return nil, err
			}
		default:
			problems = append(problems, fmt.Sprintf("slug %q in project %q is indexed to agent %s but live agent %s records it", r.AgentSlug, r.ProjectID, holder, r.AgentID))
		}
	}
	// Every index entry must name this slug's record. An entry whose
	// record is deleted with every object confirmed absent is released (the
	// ReleaseSlug rule: a crash between the tombstone and the release left
	// it); one naming a missing, unreadable or differently-slugged record is
	// reported, never reassigned.
	projects, err := os.ReadDir(s.dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.dir, p.Name(), "by-slug"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("slug index of project %q unreadable: %v", p.Name(), err))
			continue
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			slug := e.Name()
			if _, claimed := live[p.Name()+"\x00"+slug]; claimed {
				continue // checked against its live record above
			}
			holder, err := s.slugHolder(p.Name(), slug)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			if !validOwnershipElement(holder) {
				problems = append(problems, fmt.Sprintf("slug %q in project %q is indexed to an invalid agent ID %q", slug, p.Name(), holder))
				continue
			}
			rec, found, err := s.readFile(p.Name(), holder)
			switch {
			case err != nil:
				problems = append(problems, err.Error())
			case !found:
				problems = append(problems, fmt.Sprintf("slug %q in project %q is indexed to agent %s, which has no record", slug, p.Name(), holder))
			case rec.AgentSlug != slug:
				problems = append(problems, fmt.Sprintf("slug %q in project %q is indexed to agent %s, whose record names slug %q", slug, p.Name(), holder, rec.AgentSlug))
			case rec.State == OwnershipStateDeleted && allResourcesAbsent(rec):
				sp, _ := s.slugPath(p.Name(), slug)
				if err := os.Remove(sp); err != nil && !errors.Is(err, os.ErrNotExist) {
					return nil, err
				}
				syncDirBestEffort(filepath.Dir(sp))
			}
		}
	}
	return problems, nil
}

// allResourcesAbsent reports whether every object the record lists is
// confirmed absent.
func allResourcesAbsent(r *OwnershipRecord) bool {
	for _, run := range r.Runs {
		for _, res := range run.Resources {
			if res.State != OwnedResourceAbsent {
				return false
			}
		}
	}
	return true
}

// List returns every record (tombstones included), sorted by key. An
// unreadable record is returned as an error, never skipped.
func (s *OwnershipStore) List() ([]OwnershipRecord, error) {
	projects, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []OwnershipRecord
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.dir, p.Name()))
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			agentID, ok := strings.CutSuffix(f.Name(), ".json")
			if f.IsDir() || !ok || strings.HasPrefix(f.Name(), ".") {
				continue
			}
			l := s.keyLock(p.Name(), agentID)
			l.Lock()
			rec, found, err := s.readFile(p.Name(), agentID)
			l.Unlock()
			if err != nil {
				return nil, err
			}
			if found {
				out = append(out, *rec)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ProjectID != out[j].ProjectID {
			return out[i].ProjectID < out[j].ProjectID
		}
		return out[i].AgentID < out[j].AgentID
	})
	return out, nil
}

func mustJSON(v any) []byte {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err) // the record types always marshal
	}
	return data
}

// writeOwnershipFile writes data atomically: temp file, fsync, rename, then
// fsync of the directory (and of a directory it had to create), so a new
// file's directory entry is durable too.
func writeOwnershipFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	created, err := mkdirAllSynced(dir)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	syncDirBestEffort(dir)
	for _, d := range created {
		syncDirBestEffort(filepath.Dir(d))
	}
	return nil
}

// mkdirAllSynced creates dir (0700) and returns the directories it created.
func mkdirAllSynced(dir string) ([]string, error) {
	var created []string
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(d); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		created = append(created, d)
		if filepath.Dir(d) == d {
			break
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return created, nil
}

// syncDirBestEffort fsyncs the directory dir, ignoring any error.
func syncDirBestEffort(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// HasLiveAgents reports whether a live or deleting record of this instance
// is in the project. An unreadable record is an error.
func (s *OwnershipStore) HasLiveAgents(projectID string) (bool, error) {
	recs, err := s.List()
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.ProjectID == projectID && r.State != OwnershipStateDeleted {
			return true, nil
		}
	}
	return false, nil
}

// RecordsResource reports whether a live or deleting record of this
// instance recorded the object with this UID and its absence has not been
// established. Records of conflicting keys never count. An unreadable
// record is an error. It reads every record on each call (one call per
// cleanup handle), which is fine at today's per-instance scale; an index
// by UID is the change to make if cleanup volume grows.
func (s *OwnershipStore) RecordsResource(uid string) (bool, error) {
	if uid == "" {
		return false, nil
	}
	recs, err := s.List()
	if err != nil {
		return false, err
	}
	for _, r := range recs {
		if r.State == OwnershipStateDeleted || s.agentConflict(r.ProjectID, r.AgentID) != nil {
			continue
		}
		for _, run := range r.Runs {
			for _, res := range run.Resources {
				if res.UID == uid && res.State == OwnedResourceRecorded {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// launchHandleOwned is a flat instance's CleanupLaunch ownership check: the
// object is in its records, or in the journal of one of its own launches
// whose recording failed (unmirroredUIDs, registered by
// completeOwnedStart). Anything else is not deleted.
func (s *Server) launchHandleOwned(h api.ResourceHandle) bool {
	if s.ownership == nil || h.UID == "" {
		return false
	}
	if _, ok := s.unmirroredUIDs.Load(h.UID); ok {
		return true
	}
	ok, err := s.ownership.RecordsResource(h.UID)
	if err != nil {
		s.agentLifecycleLog.Warn("Launch cleanup: ownership records unreadable; not deleting the object", "uid", h.UID, "name", h.Name, "error", err)
		return false
	}
	return ok
}

// fileAgentOwned reports whether this flat instance's durable record claims
// the agent holding slug in the project (the authority for file-only
// agents). Missing, unreadable or deleted records are not owned.
func (s *Server) fileAgentOwned(projectID, slug string) bool {
	if s.ownership == nil || projectID == "" {
		return false
	}
	agentID, err := s.ownership.SlugHolder(projectID, slug)
	if err != nil || agentID == "" {
		return false
	}
	rec, ok, err := s.ownership.Get(projectID, agentID)
	return err == nil && ok && rec.State == OwnershipStateActive
}

// beginOwnedRun records a flat instance's run before the operation creates
// any agent file or runtime object. A legacy Runtime Broker records nothing.
// A create may start a new record; any other operation (start, restart of
// an existing agent) requires the agent's existing live record, so an agent
// whose ownership is not established is never adopted by starting it. A
// missing project or agent ID, a slug held by another agent (of this
// instance or of another instance of the host), or a write failure refuses
// the operation before any side effect.
func (s *Server) beginOwnedRun(projectID, agentID, slug, runID string, create bool) error {
	if s.ownership == nil {
		return nil
	}
	if projectID == "" || agentID == "" {
		return fmt.Errorf("flat Runtime Broker %s: a project ID and an agent ID are required to record ownership", s.ownership.RuntimeBrokerID())
	}
	if !create {
		rec, ok, err := s.ownership.Get(projectID, agentID)
		if err != nil {
			return err
		}
		if !ok || rec.State != OwnershipStateActive {
			return fmt.Errorf("%w: agent %s in project %s has no live ownership record of this Runtime Broker instance", ErrOwnershipNotRecorded, agentID, projectID)
		}
	}
	// The slug must also be free on every other instance sharing this
	// host's project directories.
	return s.locks().reserveSlug(s, projectID, slug, func() error {
		return s.ownership.BeginRun(projectID, agentID, slug, runID)
	})
}

// ownedStart is one flat start's ownership journal mirror: it records every
// created runtime object (the runtime's ObserveResourceCreated) into the
// durable record and latches the first failure. The in-memory handles are
// the trusted current launch journal for cleanup when mirroring failed.
type ownedStart struct {
	store                     *OwnershipStore
	projectID, agentID, runID string

	mu      sync.Mutex
	err     error
	handles []api.ResourceHandle
}

func (o *ownedStart) observe(h api.ResourceHandle) {
	o.mu.Lock()
	o.handles = append(o.handles, h)
	failed := o.err != nil
	o.mu.Unlock()
	if failed {
		return
	}
	if err := o.store.AddResource(o.projectID, o.agentID, o.runID, h); err != nil {
		o.mu.Lock()
		if o.err == nil {
			o.err = fmt.Errorf("recording runtime object %s (%s) of run %s: %w", h.Name, h.UID, o.runID, err)
		}
		o.mu.Unlock()
	}
}

func (o *ownedStart) latched() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *ownedStart) snapshot() []api.ResourceHandle {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]api.ResourceHandle(nil), o.handles...)
}

// checkpoint refuses the next resource-creating call once mirroring failed.
func (o *ownedStart) checkpoint(context.Context, string) error {
	if err := o.latched(); err != nil {
		return fmt.Errorf("flat Runtime Broker: not creating more runtime objects: %w", err)
	}
	return nil
}

// chainCheckpoint runs first, then second (either may be nil).
func chainCheckpoint(first, second func(context.Context, string) error) func(context.Context, string) error {
	switch {
	case first == nil:
		return second
	case second == nil:
		return first
	}
	return func(ctx context.Context, step string) error {
		if err := first(ctx, step); err != nil {
			return err
		}
		return second(ctx, step)
	}
}

// installOwnedStart wires a flat start's ownership mirror into opts: the
// observer and a checkpoint that refuses further creates after a mirroring
// failure (chained before any checkpoint already set). It returns the
// installed mirror, or nil when none was installed.
func (s *Server) installOwnedStart(opts *api.StartOptions, projectID, agentID string) *ownedStart {
	if s.ownership == nil || opts.RunID == "" {
		return nil
	}
	o := &ownedStart{store: s.ownership, projectID: projectID, agentID: agentID, runID: opts.RunID}
	s.ownedStarts.Store(opts.RunID, o)
	opts.ObserveResourceCreated = o.observe
	opts.Checkpoint = chainCheckpoint(o.checkpoint, opts.Checkpoint)
	return o
}

// completeOwnedStart finishes a flat start's ownership mirror after
// Manager.Start returned. A failed start is returned as is (its run stays
// provisioning; the existing failure cleanup applies). A successful start
// whose mirroring failed (including on its last object) is undone: when
// cleanup is set, exactly the journaled objects are removed with their UID
// preconditions (the synchronous path); the async path's own failure
// cleanup removes them otherwise. Only then is the run marked created.
func (s *Server) completeOwnedStart(ctx context.Context, mgr agent.Manager, runID string, startErr error, cleanup bool) error {
	v, ok := s.ownedStarts.LoadAndDelete(runID)
	if !ok {
		return startErr
	}
	o := v.(*ownedStart)
	if o.latched() != nil {
		// Recording failed, so the record may lack some of these objects;
		// the launch's own journal makes them this instance's to clean up.
		// The creating run is kept with each UID for diagnosis.
		for _, h := range o.snapshot() {
			if h.UID != "" {
				s.unmirroredUIDs.Store(h.UID, ownedRunKey{projectID: o.projectID, agentID: o.agentID, runID: o.runID})
			}
		}
	}
	if startErr != nil {
		// A failed synchronous start: the runtime cleaned up after itself;
		// confirm exactly the journaled objects are gone and finish only
		// this run. The async path finishes it after its own cleanup.
		if cleanup {
			s.cleanupOwnedRun(ctx, mgr, o.projectID, o.agentID, o.runID, o.snapshot())
		}
		return startErr
	}
	err := o.latched()
	if err == nil {
		err = o.store.SetRunState(o.projectID, o.agentID, o.runID, OwnershipStateCreated)
	}
	if err == nil {
		return nil
	}
	if cleanup {
		s.cleanupOwnedRun(ctx, mgr, o.projectID, o.agentID, o.runID, o.snapshot())
	}
	return fmt.Errorf("flat Runtime Broker: the agent's runtime objects could not be recorded, so the start was undone: %w", err)
}

// releaseOwnedStart drops a run's ownership mirror that no Start consumed
// (a provision-only create, or a request that ended between building the
// start context and starting). The run's record is left as it is.
func (s *Server) releaseOwnedStart(runID string) {
	if runID != "" {
		s.ownedStarts.Delete(runID)
	}
}

// releaseOwnedMirror drops the ownership mirror o for runID only if it is
// still the one installed for that run: a later start that installed its
// own mirror under the same run ID keeps it. A nil o is a no-op.
func (s *Server) releaseOwnedMirror(runID string, o *ownedStart) {
	if runID != "" && o != nil {
		s.ownedStarts.CompareAndDelete(runID, o)
	}
}

// ownerScopedManager is a manager a flat instance can restrict to its own
// objects (agent.AgentManager).
type ownerScopedManager interface {
	SetOwner(agent.OwnerScope)
}

// ownedRunKey identifies a flat instance's run in its ownership records.
type ownedRunKey struct {
	projectID, agentID, runID string
}

// ownedRunKeyFor returns the ownership key of an in-flight owned start (the
// zero key when the run has none, e.g. a legacy Runtime Broker).
func (s *Server) ownedRunKeyFor(runID string) ownedRunKey {
	if v, ok := s.ownedStarts.Load(runID); ok {
		o := v.(*ownedStart)
		return ownedRunKey{projectID: o.projectID, agentID: o.agentID, runID: o.runID}
	}
	return ownedRunKey{}
}

// cleanupOwnedRun removes a failed run's journaled objects with their UID
// preconditions (an object already gone counts as removed) and then
// finishes that run only (finishCleanedRun); the record and every other run
// stay as they are. If the removal fails, the run is left deleting with its
// objects recorded, for the next delete or reconciliation.
func (s *Server) cleanupOwnedRun(ctx context.Context, mgr agent.Manager, projectID, agentID, runID string, handles []api.ResourceHandle) {
	if s.ownership == nil {
		return
	}
	if len(handles) > 0 {
		if mgr == nil {
			_ = s.ownership.SetRunState(projectID, agentID, runID, OwnershipStateDeleting)
			return
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := mgr.CleanupLaunch(cctx, handles); err != nil {
			s.agentLifecycleLog.Error("Cleaning up a failed run failed; its objects stay recorded and labelled for reconciliation",
				"run_id", runID, "error", err)
			_ = s.ownership.SetRunState(projectID, agentID, runID, OwnershipStateDeleting)
			return
		}
	}
	s.finishCleanedRun(ownedRunKey{projectID: projectID, agentID: agentID, runID: runID}, handles)
}

// finishCleanedRun records that a run's objects were removed (each handle
// confirmed absent) and moves only that run to deleting and deleted.
func (s *Server) finishCleanedRun(k ownedRunKey, handles []api.ResourceHandle) {
	if s.ownership == nil || k.runID == "" {
		return
	}
	if err := s.ownership.SetRunState(k.projectID, k.agentID, k.runID, OwnershipStateDeleting); err != nil {
		return
	}
	// A delete request is not absence: the run is finished only when every
	// object is confirmed gone; otherwise it stays deleting with the
	// unconfirmed objects recorded, for the next delete or reconciliation.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if !s.markConfirmedAbsent(ctx, k.projectID, k.agentID, handles) {
		return
	}
	if err := s.ownership.SetRunState(k.projectID, k.agentID, k.runID, OwnershipStateDeleted); err != nil {
		s.agentLifecycleLog.Warn("A cleaned-up run was not marked deleted", "run_id", k.runID, "error", err)
	}
}

// markConfirmedAbsent checks each handle with the runtime's exact absence
// check and marks the confirmed ones absent. It reports whether every
// handle is confirmed gone. Without the capability, or on a check error or
// an object still present (terminating included), that handle stays
// recorded.
func (s *Server) markConfirmedAbsent(ctx context.Context, projectID, agentID string, handles []api.ResourceHandle) bool {
	checker, _ := s.currentRuntime().(scionrt.ResourceAbsenceChecker)
	all := true
	for _, h := range handles {
		if h.UID == "" {
			continue
		}
		absent := false
		if checker != nil {
			gone, err := checker.ResourceAbsent(ctx, h)
			if err != nil {
				s.agentLifecycleLog.Warn("Could not establish whether an object is gone; it stays recorded and is checked again by the next delete or start-up",
					"agent_id", agentID, "project_id", projectID, "kind", h.Kind, "namespace", h.Namespace, "name", h.Name, "uid", h.UID, "error", err)
			}
			absent = err == nil && gone
		}
		if !absent {
			all = false
			continue
		}
		if err := s.ownership.MarkAbsent(projectID, agentID, h.UID); err != nil {
			return false
		}
	}
	return all
}

// Reconstruct ensures the record of a runtime object that carries this
// instance's owner label: an object whose labels give the complete project
// ID, agent ID, slug and run ID, and which has an object identity (its
// container or pod ID), is recorded (record and run created as needed, the
// object added) without reviving a terminal run. Missing metadata, or a
// record that contradicts the labels, is an error (unresolved).
func (s *OwnershipStore) Reconstruct(o api.AgentInfo) error {
	if o.Labels[api.LabelRuntimeBrokerID] != s.brokerID {
		return fmt.Errorf("object %s is not labelled for this instance", o.Name)
	}
	projectID := o.Labels["scion.project_id"]
	agentID := o.Labels["agent_id"]
	slug := o.Labels["scion.name"]
	runID := o.Labels[api.LabelRunID]
	uid := o.ContainerID
	if uid == "" {
		uid = o.ID
	}
	if reconstructedKind(o) == api.ResourceKindPod && o.Kubernetes != nil && o.Kubernetes.UID != "" {
		uid = o.Kubernetes.UID
	}
	if projectID == "" || agentID == "" || slug == "" || runID == "" || uid == "" {
		return fmt.Errorf("labels are incomplete (project, agent, name, run and object ID are all required)")
	}
	rec, ok, err := s.Get(projectID, agentID)
	if err != nil {
		return err
	}
	if ok {
		if rec.AgentSlug != slug {
			return fmt.Errorf("record names slug %q, the object %q", rec.AgentSlug, slug)
		}
		// An object of an agent (or run) whose delete started but did not
		// finish is known and pending deletion: nothing is appended, and the
		// instance is not refused; the delete retry removes it.
		if rec.State == OwnershipStateDeleting {
			return nil
		}
		if run := rec.Run(runID); run != nil {
			if run.State == OwnershipStateDeleting {
				return nil
			}
			if runStateOrder[run.State] >= runStateOrder[OwnershipStateDeleting] {
				return fmt.Errorf("run %s is %s and cannot be revived", runID, run.State)
			}
			if rec.OwnsUID(uid) || !reconstructsHandle(o) {
				return nil
			}
			return s.AddResource(projectID, agentID, runID, reconstructedHandle(o, uid))
		}
	}
	if err := s.BeginRun(projectID, agentID, slug, runID); err != nil {
		return err
	}
	if reconstructsHandle(o) {
		if err := s.AddResource(projectID, agentID, runID, reconstructedHandle(o, uid)); err != nil {
			return err
		}
	}
	return s.SetRunState(projectID, agentID, runID, OwnershipStateCreated)
}

// reconstructsHandle reports whether a listed object identifies itself well
// enough to be recorded as a cleanup handle. A container's ID is its
// identity. A pod is recorded only with both its immutable UID and its
// namespace (api.AgentInfo.Kubernetes, set by the Kubernetes runtime's
// List); a handle built from the pod
// name would let a UID precondition delete report the pod gone without
// checking it, so without them the object only re-establishes the record
// and run (the pod is still found through the owner-filtered list and
// deleted by the agent delete).
func reconstructsHandle(o api.AgentInfo) bool {
	if reconstructedKind(o) == api.ResourceKindPod {
		return o.Kubernetes != nil && o.Kubernetes.UID != "" && o.Kubernetes.Namespace != ""
	}
	return true
}

// reconstructedHandle is the cleanup handle of a listed object.
func reconstructedHandle(o api.AgentInfo, uid string) api.ResourceHandle {
	h := api.ResourceHandle{Kind: reconstructedKind(o), Name: o.Name, UID: uid}
	if h.Kind == api.ResourceKindPod && o.Kubernetes != nil {
		h.Name, h.Namespace = o.ContainerID, o.Kubernetes.Namespace
		if o.Kubernetes.PodName != "" {
			h.Name = o.Kubernetes.PodName
		}
	}
	return h
}

// ReconcileAbsent records what the start-up read of the instance's
// execution scope proves gone and finishes what that allows. present is a
// COMPLETE read of every scion agent object (the caller must not call it
// after a failed or partial read: an inspection failure is never absence);
// absent, when set, is the runtime's exact absence check by immutable
// identity (runtime.ResourceAbsenceChecker); warn receives check failures.
// For every live or deleting record of this instance (not conflicting):
//   - a recorded main object (container or pod) that a listed object
//     matches is present (matching is deliberately loose: any of the
//     object's IDs, by prefix, or its name). One that no listed object
//     matches is marked absent only when absent confirms it, since the
//     listing may not cover every namespace a pod was created in; without
//     absent the listing alone decides.
//   - any other recorded object (children, and main objects of a deleting
//     run or record) is checked with absent; only a confirmed absence marks
//     it, and a check failure keeps it recorded (retried next time).
//   - a deleting run whose objects are all absent is finished as deleted;
//     a deleting record whose files were removed and whose objects are all
//     absent is finished as deleted and its slug released.
//   - an older provisioning run (not the record's latest run) that
//     recorded no object still present is finished as deleted: its start
//     ended without completing. The latest run is kept, since it may be a
//     created agent that has not started yet.
//
// It returns the number of changes it made.
func (s *OwnershipStore) ReconcileAbsent(present []api.AgentInfo, absent func(api.ResourceHandle) (bool, error), warn func(msg string, args ...any)) (int, error) {
	recs, err := s.List()
	if err != nil {
		return 0, err
	}
	changed := 0
	for _, r := range recs {
		if r.State == OwnershipStateDeleted || s.agentConflict(r.ProjectID, r.AgentID) != nil {
			continue
		}
		for _, run := range r.Runs {
			for _, res := range run.Resources {
				if res.State != OwnedResourceRecorded {
					continue
				}
				main := res.Kind == api.ResourceKindContainer || res.Kind == api.ResourceKindPod
				listed := main && objectListed(res, present)
				if main && !listed && absent == nil {
					// No exact check: the listing alone decides (callers
					// without a runtime check; a flat instance always has one).
					if err := s.MarkAbsent(r.ProjectID, r.AgentID, res.UID); err != nil {
						return changed, err
					}
					changed++
					continue
				}
				if absent == nil || (listed && r.State == OwnershipStateActive && runStateOrder[run.State] < runStateOrder[OwnershipStateDeleting]) {
					continue // a listed main object of a live run is present
				}
				// Every other object, an unlisted main object included, is
				// confirmed gone only by the exact check: a listing may not
				// cover every namespace this instance creates objects in.
				h := api.ResourceHandle{Kind: res.Kind, Namespace: res.Namespace, Name: res.Name, UID: res.UID}
				gone, err := absent(h)
				if err != nil {
					if warn != nil {
						warn("Could not establish whether a recorded object is gone; it stays recorded", "agent_id", r.AgentID, "project_id", r.ProjectID,
							"kind", res.Kind, "namespace", res.Namespace, "name", res.Name, "uid", res.UID, "error", err)
					}
					continue
				}
				if gone {
					if err := s.MarkAbsent(r.ProjectID, r.AgentID, res.UID); err != nil {
						return changed, err
					}
					changed++
				}
			}
		}
		cur, ok, err := s.Get(r.ProjectID, r.AgentID)
		if err != nil {
			return changed, err
		}
		if !ok {
			continue
		}
		for i, run := range cur.Runs {
			switch {
			case run.State == OwnershipStateDeleting && !runHasRecordedObject(&cur.Runs[i]):
				if err := s.SetRunState(r.ProjectID, r.AgentID, run.RunID, OwnershipStateDeleted); err != nil {
					return changed, err
				}
				changed++
			case run.State == OwnershipStateProvisioning && i != len(cur.Runs)-1 && !runHasRecordedObject(&cur.Runs[i]):
				if err := s.SetRunState(r.ProjectID, r.AgentID, run.RunID, OwnershipStateDeleted); err != nil {
					return changed, err
				}
				changed++
			}
		}
		if cur.State == OwnershipStateDeleting && cur.FilesRemoved && allResourcesAbsent(cur) {
			if err := s.SetRecordState(r.ProjectID, r.AgentID, OwnershipStateDeleted); err != nil {
				return changed, err
			}
			if err := s.ReleaseSlug(r.ProjectID, r.AgentID); err != nil {
				return changed, err
			}
			changed++
		}
	}
	return changed, nil
}

// ReconstructChild records a launch child object (found by the instance's
// owner label) whose labels identify it completely: owner, project ID,
// agent ID, slug and run, plus its kind, namespace, name and UID. It covers
// a crash after the child was created but before its handle was recorded,
// with or without a main object. A child of a deleting record or run is
// known and pending deletion; a child of a finished run, or one whose labels
// are incomplete or contradict the record, is unresolved (an error).
func (s *OwnershipStore) ReconstructChild(labels map[string]string, h api.ResourceHandle) error {
	if labels[api.LabelRuntimeBrokerID] != s.brokerID {
		return fmt.Errorf("object %s is not labelled for this instance", h.Name)
	}
	projectID, agentID := labels["scion.project_id"], labels[api.LabelAgentID]
	slug, runID := labels["scion.name"], labels[api.LabelRunID]
	if projectID == "" || agentID == "" || slug == "" || runID == "" || h.UID == "" || h.Name == "" || h.Kind == "" {
		return fmt.Errorf("child %s %s/%s: labels are incomplete (project, agent, name and run are all required)", h.Kind, h.Namespace, h.Name)
	}
	rec, ok, err := s.Get(projectID, agentID)
	if err != nil {
		return err
	}
	if ok {
		if rec.AgentSlug != slug {
			return fmt.Errorf("record names slug %q, the child %q", rec.AgentSlug, slug)
		}
		if rec.State == OwnershipStateDeleting {
			return nil
		}
		if run := rec.Run(runID); run != nil {
			if run.State == OwnershipStateDeleting {
				return nil
			}
			if runStateOrder[run.State] >= runStateOrder[OwnershipStateDeleted] {
				return fmt.Errorf("child %s of run %s, which is %s", h.Name, runID, run.State)
			}
			if rec.OwnsUID(h.UID) {
				return nil
			}
			return s.AddResource(projectID, agentID, runID, h)
		}
	}
	if err := s.BeginRun(projectID, agentID, slug, runID); err != nil {
		return err
	}
	return s.AddResource(projectID, agentID, runID, h)
}

// objectListed reports whether any listed object may be the recorded one.
func objectListed(res OwnedResource, present []api.AgentInfo) bool {
	sameID := func(a, b string) bool {
		if a == "" || b == "" {
			return false
		}
		if len(a) >= 12 && len(b) >= 12 {
			return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
		}
		return a == b
	}
	for _, o := range present {
		ids := []string{o.ID, o.ContainerID, o.Labels["scion.container.id"]}
		if o.Kubernetes != nil {
			ids = append(ids, o.Kubernetes.UID, o.Kubernetes.PodName)
		}
		for _, id := range ids {
			if sameID(res.UID, id) || (res.Name != "" && res.Name == id) {
				return true
			}
		}
		if res.Name != "" && res.Name == o.Name {
			return true
		}
	}
	return false
}

func runHasRecordedObject(run *OwnedRun) bool {
	if run == nil {
		return true
	}
	for _, res := range run.Resources {
		if res.State == OwnedResourceRecorded {
			return true
		}
	}
	return false
}

// reconstructedKind is the resource kind of a listed agent object.
func reconstructedKind(o api.AgentInfo) string {
	if o.Runtime == "kubernetes" {
		return api.ResourceKindPod
	}
	return api.ResourceKindContainer
}

// ownedDelete is a flat instance's delete of an owned agent: either the
// whole record (every run; the slug is released at the end) or exactly one
// run (that run's objects only; the record and the other runs untouched).
type ownedDelete struct {
	projectID, agentID string
	// runID, when wholeRecord is false, is the only run the delete acts on.
	runID       string
	wholeRecord bool
	// filesRemoved is set by the caller once the runtime delete succeeded
	// and the agent's files were removed (or there were none to remove).
	filesRemoved bool
}

// ownedDeleteTarget returns the live record of the agent holding slug in
// the project (nil when there is none or it is a tombstone) and refuses a
// file-only agent (no runtime object) this instance's record does not own.
// A legacy Runtime Broker (no store) is always allowed. An agent with a
// runtime object was found through this instance's owner-filtered list, so
// its label already establishes ownership even when its record is missing
// (the next start-up reconstructs it).
func (s *Server) ownedDeleteTarget(projectID, slug string, hasObject bool) (*OwnershipRecord, error) {
	if s.ownership == nil {
		return nil, nil
	}
	var rec *OwnershipRecord
	if projectID != "" {
		agentID, err := s.ownership.SlugHolder(projectID, slug)
		if err != nil {
			return nil, err
		}
		if agentID != "" {
			r, ok, err := s.ownership.Get(projectID, agentID)
			if err != nil {
				return nil, err
			}
			if ok {
				rec = r
			}
		}
	}
	if rec == nil || rec.State == OwnershipStateDeleted {
		if !hasObject {
			return nil, fmt.Errorf("%w: file-only agent %q in project %q", ErrOwnershipNotRecorded, slug, projectID)
		}
		return nil, nil
	}
	return rec, nil
}

// finishPendingOwnedDelete finishes the record of the agent holding slug
// when its caller found neither a runtime entry nor files for the agent,
// which proves the files are gone. It acts on:
//   - a record already deleting: an earlier whole-agent delete whose
//     objects were not yet all confirmed gone, retried;
//   - for a whole-agent delete (wholeAgent), an active record with no live
//     run: an agent whose every run already ended, such as a create that
//     failed and had its run cleaned up before the Hub's rollback delete
//     arrived. It is moved to deleting first (RetireFinishedRecord), so
//     the slug is not left reserved by an agent the Hub no longer has.
//
// It records that the files are gone, cleans up the still-recorded objects
// again (UID preconditions) and finishes the record once they are confirmed
// gone. It does nothing for a legacy Runtime Broker, when no record holds
// the slug, or for a record with a live run.
func (s *Server) finishPendingOwnedDelete(ctx context.Context, projectID, slug string, wholeAgent bool) {
	if s.ownership == nil || projectID == "" {
		return
	}
	agentID, err := s.ownership.SlugHolder(projectID, slug)
	if err != nil || agentID == "" {
		return
	}
	rec, ok, err := s.ownership.Get(projectID, agentID)
	if err != nil || !ok {
		return
	}
	switch {
	case rec.State == OwnershipStateDeleting:
	case rec.State == OwnershipStateActive && wholeAgent:
		retired, err := s.ownership.RetireFinishedRecord(projectID, agentID)
		if err != nil {
			s.agentLifecycleLog.Warn("Agent delete: an ended agent's ownership record was not moved to deleting",
				"agent_id", agentID, "project_id", projectID, "error", err)
			return
		}
		if !retired {
			return
		}
	default:
		return
	}
	s.finishOwnedDelete(ctx, s.currentManager(), &ownedDelete{projectID: projectID, agentID: agentID, wholeRecord: true, filesRemoved: true})
}

// leftoverCleanupAllowed reports whether a name-scoped leftover cleanup may
// run for the agent holding slug in the project: always for a legacy
// Runtime Broker; for a flat instance only when its record (live or
// deleting) holds the slug and, for a delete fenced to runID, no other run
// of the agent is live.
func (s *Server) leftoverCleanupAllowed(projectID, slug, runID string) bool {
	if s.ownership == nil {
		return true
	}
	agentID, err := s.ownership.SlugHolder(projectID, slug)
	if err != nil || agentID == "" {
		return false
	}
	rec, ok, err := s.ownership.Get(projectID, agentID)
	if err != nil || !ok || rec.State == OwnershipStateDeleted {
		return false
	}
	return runID == "" || !liveRunsOtherThan(rec, runID)
}

// checkOwnedDelete is the ownership check a delete passes before any side
// effect; it changes nothing.
func (s *Server) checkOwnedDelete(projectID, slug string, hasObject bool) error {
	_, err := s.ownedDeleteTarget(projectID, slug, hasObject)
	return err
}

// liveRunsOtherThan reports whether the record has a live (provisioning or
// created) run other than runID.
func liveRunsOtherThan(rec *OwnershipRecord, runID string) bool {
	for _, r := range rec.Runs {
		if r.RunID != runID && runStateOrder[r.State] < runStateOrder[OwnershipStateDeleting] {
			return true
		}
	}
	return false
}

// beginOwnedDelete moves the record or run a delete acts on to deleting
// before anything is removed, and returns what finishOwnedDelete completes.
//
// A delete fenced to a run (runID) never acts on another run: it moves only
// that run to deleting when another run is live or the delete is not a
// whole-agent delete, and does nothing to the record when it names a run
// the record does not have while another run is live. Only a whole-agent
// delete with no other live run (or an unfenced whole-agent delete, which
// means the agent itself) moves the whole record to deleting. A record that
// is already deleting (a retried delete) continues as a whole-record delete.
func (s *Server) beginOwnedDelete(projectID, slug, runID string, hasObject, wholeAgent bool) (*ownedDelete, error) {
	rec, err := s.ownedDeleteTarget(projectID, slug, hasObject)
	if err != nil || rec == nil {
		return nil, err
	}
	scope, err := s.ownership.BeginDelete(projectID, rec.AgentID, runID, wholeAgent)
	switch {
	case err != nil:
		return nil, err
	case scope == deleteScopeRun:
		return &ownedDelete{projectID: projectID, agentID: rec.AgentID, runID: runID}, nil
	case scope == deleteScopeRecord:
		return &ownedDelete{projectID: projectID, agentID: rec.AgentID, wholeRecord: true}, nil
	}
	return nil, nil
}

// deleteScope is what a delete acts on (BeginDelete).
type deleteScope int

const (
	deleteScopeNone deleteScope = iota
	deleteScopeRun
	deleteScopeRecord
)

// BeginDelete decides what a delete acts on and moves it to deleting, in
// one step under the record's key lock, so a concurrent BeginRun cannot
// land between the decision and the transition:
//   - a delete fenced to a run (runID) acts on that run only when another
//     run is live or the delete is not a whole-agent delete; it does
//     nothing when it names a run the record does not have while another
//     run is live, or a run already deleted;
//   - otherwise a whole-agent delete moves the whole record to deleting
//     (an already deleting record continues as a whole-record delete);
//   - anything else acts on nothing.
func (s *OwnershipStore) BeginDelete(projectID, agentID, runID string, wholeAgent bool) (deleteScope, error) {
	scope := deleteScopeNone
	_, err := s.mutate(projectID, agentID, func(r *OwnershipRecord) error {
		scope = deleteScopeNone
		if r.State == OwnershipStateActive && runID != "" {
			otherLive := liveRunsOtherThan(r, runID)
			if run := r.Run(runID); run == nil {
				if otherLive || !wholeAgent {
					return errNoChange
				}
			} else if otherLive || !wholeAgent {
				if runStateOrder[run.State] >= runStateOrder[OwnershipStateDeleted] {
					return errNoChange
				}
				if runStateOrder[run.State] < runStateOrder[OwnershipStateDeleting] {
					run.State = OwnershipStateDeleting
				}
				scope = deleteScopeRun
				return nil
			}
		}
		if !wholeAgent {
			return errNoChange
		}
		if recordStateOrder[r.State] > recordStateOrder[OwnershipStateDeleting] {
			return fmt.Errorf("%w: agent %q from %s to %s", ErrOwnershipStateOrder, agentID, r.State, OwnershipStateDeleting)
		}
		r.State = OwnershipStateDeleting
		scope = deleteScopeRecord
		return nil
	})
	if errors.Is(err, errNoChange) {
		return deleteScopeNone, nil
	}
	return scope, err
}

// RetireFinishedRecord moves an active record with no live run (every run
// deleting or deleted) to deleting, for a whole-agent delete that found
// nothing left of the agent. It reports whether it did; a record with a
// live run, or one not active, is left unchanged. The check and the move
// happen under the record's key lock, so a concurrent BeginRun cannot land
// between them.
func (s *OwnershipStore) RetireFinishedRecord(projectID, agentID string) (bool, error) {
	_, err := s.mutate(projectID, agentID, func(r *OwnershipRecord) error {
		if r.State != OwnershipStateActive || liveRunsOtherThan(r, "") {
			return errNoChange
		}
		r.State = OwnershipStateDeleting
		return nil
	})
	if errors.Is(err, errNoChange) {
		return false, nil
	}
	return err == nil, err
}

// errNoChange makes mutate leave a record unwritten.
var errNoChange = errors.New("no change")

// finishOwnedDelete completes a delete after the runtime delete succeeded.
// The objects in scope (every run's for a whole-record delete, the one
// run's otherwise) are removed or confirmed gone through the UID-precondition
// cleanup (an object already gone, or a name now held by another UID, is
// confirmed absent). Then a run delete marks that run deleted; a whole-record
// delete marks the record deleted (its tombstone stays) and releases the
// slug. If absence cannot be established the record or run stays deleting
// and the slug stays reserved, so nothing reuses it early.
func (s *Server) finishOwnedDelete(ctx context.Context, mgr agent.Manager, od *ownedDelete) {
	if od == nil || s.ownership == nil {
		return
	}
	rec, ok, err := s.ownership.Get(od.projectID, od.agentID)
	if err != nil || !ok {
		return
	}
	var handles []api.ResourceHandle
	for _, run := range rec.Runs {
		if !od.wholeRecord && run.RunID != od.runID {
			continue
		}
		for _, res := range run.Resources {
			if res.State == OwnedResourceRecorded {
				handles = append(handles, api.ResourceHandle{Kind: res.Kind, Namespace: res.Namespace, Name: res.Name, UID: res.UID})
			}
		}
	}
	// Recorded first: the files are gone whatever happens to the cleanup
	// below, so a retry or start-up can finish the record once its objects
	// are confirmed gone.
	if od.wholeRecord && od.filesRemoved && !rec.FilesRemoved {
		if err := s.ownership.MarkFilesRemoved(od.projectID, od.agentID); err != nil {
			return
		}
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if len(handles) > 0 && mgr != nil {
		if err := mgr.CleanupLaunch(cctx, handles); err != nil {
			s.agentLifecycleLog.Warn("Agent delete: could not remove every recorded object; ownership record kept deleting",
				"agent_id", od.agentID, "project_id", od.projectID, "run_id", od.runID, "error", err)
			return
		}
	}
	// A delete request is not absence (a deletion may still be pending):
	// only objects confirmed gone are marked absent, and the run or record
	// is finished only when every one is.
	if !s.markConfirmedAbsent(cctx, od.projectID, od.agentID, handles) {
		s.agentLifecycleLog.Info("Agent delete: not every recorded object is confirmed gone yet; ownership record kept deleting",
			"agent_id", od.agentID, "project_id", od.projectID, "run_id", od.runID)
		return
	}
	if !od.wholeRecord {
		if err := s.ownership.SetRunState(od.projectID, od.agentID, od.runID, OwnershipStateDeleted); err != nil {
			s.agentLifecycleLog.Warn("Agent delete: run not marked deleted", "agent_id", od.agentID, "project_id", od.projectID, "run_id", od.runID, "error", err)
		}
		return
	}
	if err := s.ownership.SetRecordState(od.projectID, od.agentID, OwnershipStateDeleted); err != nil {
		return
	}
	if err := s.ownership.ReleaseSlug(od.projectID, od.agentID); err != nil {
		s.agentLifecycleLog.Warn("Agent delete: slug not released", "agent_id", od.agentID, "project_id", od.projectID, "error", err)
	}
}
