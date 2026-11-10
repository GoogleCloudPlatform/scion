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

package brokerconvert

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// The convert state lives in one owner-only directory under the global
// directory: <global>/runtime-brokers/.convert. Instance keys never start
// with a dot, so it cannot collide with an instance identity directory.
const (
	// ConvertDirName is the convert state directory's name inside
	// runtime-brokers/.
	ConvertDirName = ".convert"
	// JournalFileName is the journal's file name in the convert directory.
	JournalFileName = "journal.json"
	// HostLockFileName is the host lock's file name in the convert
	// directory.
	HostLockFileName = "lock"
	// EvidenceFilePrefix starts the name of the evidence copy of the
	// pre-apply settings file: settings-before-<runId>.yaml.
	EvidenceFilePrefix = "settings-before-"
	// InstancesFilePrefix starts the name of the copy of the instances an
	// apply wrote: instances-written-<runId>.yaml. Rollback removes the
	// instances key only while it still equals this copy.
	InstancesFilePrefix = "instances-written-"
	// RolledBackDirName holds the identity directories a rollback moved
	// aside, one subdirectory per run.
	RolledBackDirName = "rolled-back"
)

// JournalSchemaVersion is the version of the journal document.
const JournalSchemaVersion = 1

// maxJournalBytes bounds a journal read.
const maxJournalBytes = 4 << 20

// convertRel is the convert directory relative to the global directory,
// in the slash form privatefs takes.
var convertRel = brokeridentity.RuntimeBrokersDirName + "/" + ConvertDirName

// ConvertDir returns the convert state directory under globalDir.
func ConvertDir(globalDir string) string {
	return filepath.Join(globalDir, brokeridentity.RuntimeBrokersDirName, ConvertDirName)
}

// StepState is the state of one journal item.
type StepState string

// Item states. An item is written "started" before its effect and "done"
// after it; a crash between the two leaves it "started".
const (
	StepPending StepState = "pending"
	StepStarted StepState = "started"
	StepDone    StepState = "done"
)

// JournalState is the state of a whole run.
type JournalState string

// Run states.
const (
	// JournalApplying: an apply is in progress or was interrupted.
	JournalApplying JournalState = "applying"
	// JournalAwaitingStop: the co-located running phase is complete and
	// the server must be stopped before the next apply (reserved for the
	// co-located path).
	JournalAwaitingStop JournalState = "awaiting-stop"
	// JournalApplied: every apply step is done; the switch takes effect at
	// the next server start.
	JournalApplied JournalState = "applied"
	// JournalRollingBack: a rollback is in progress or was interrupted.
	JournalRollingBack JournalState = "rolling-back"
	// JournalRolledBack: the rollback is complete.
	JournalRolledBack JournalState = "rolled-back"
)

// ItemStatus is the state of one item and, after a failure, a fixed error
// text. Error never carries a Hub message, an OS error string or a
// settings value: only a fixed text and at most an HTTP status code.
type ItemStatus struct {
	State StepState `json:"state"`
	Error string    `json:"error,omitempty"`
}

// RegisterItem is step 1 for one instance.
type RegisterItem struct {
	Key             string `json:"key"`
	Name            string `json:"name"`
	BrokerID        string `json:"brokerId,omitempty"`
	RuntimeTargetID string `json:"runtimeTargetId,omitempty"`
	ItemStatus
	// MaxAgents is the agent limit the instance row takes over from the
	// legacy row's own override (0 is unlimited); Capacity is its write.
	MaxAgents *int64     `json:"maxAgents,omitempty"`
	Capacity  ItemStatus `json:"capacity"`
	// CredentialsCleanup is the removal of the instance credentials file
	// that a co-located apply's HTTP pre-registration leaves behind. Nil
	// when not applicable (the remote path).
	CredentialsCleanup *ItemStatus `json:"credentialsCleanup,omitempty"`
}

// ProjectItem is step 3 for one project.
type ProjectItem struct {
	ProjectID string `json:"projectId"`
	// OldDefault is the project's default Runtime Broker ID before step 3.
	OldDefault string `json:"oldDefault,omitempty"`
	// NewDefault is the default step 3 set; empty when the default did not
	// point at the legacy Runtime Broker and was left alone.
	NewDefault string `json:"newDefault,omitempty"`
	// Linked are the instance Runtime Broker IDs linked to the project.
	Linked []string `json:"linked"`
	ItemStatus
}

// RollbackProjectItem is the rollback of one project's step 3.
type RollbackProjectItem struct {
	ProjectID string `json:"projectId"`
	ItemStatus
}

// DeregisterItem is the rollback's deregistration of one instance row.
type DeregisterItem struct {
	BrokerID string `json:"brokerId"`
	ItemStatus
}

// MovedItem is the rollback's move of one instance identity directory.
type MovedItem struct {
	Key string `json:"key"`
	ItemStatus
}

// RollbackRecord is the rollback part of the journal.
type RollbackRecord struct {
	Projects   []RollbackProjectItem `json:"projects"`
	Deregister []DeregisterItem      `json:"deregister"`
	Settings   ItemStatus            `json:"settings"`
	Moved      []MovedItem           `json:"moved"`
}

// Journal is the local record of one convert run. It holds keys, names,
// IDs, states and fixed texts only: never an instance configuration, an
// env value or a secret target.
type Journal struct {
	SchemaVersion int `json:"schemaVersion"`
	// RunID is random and local; it names the rolled-back directory.
	RunID string `json:"runId"`
	// PlanDigest is PlanDigest of the plan the run applies.
	PlanDigest     string       `json:"planDigest"`
	LegacyBrokerID string       `json:"legacyBrokerId"`
	State          JournalState `json:"state"`
	// EvidenceFile is the evidence copy's file name in the convert
	// directory.
	EvidenceFile string `json:"evidenceFile,omitempty"`
	// InstancesFile is the file name, in the convert directory, of the
	// copy of the instances step 2 writes. Like the evidence copy it holds
	// settings values, owner-only, and is never printed or sent.
	InstancesFile string    `json:"instancesFile,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`

	Register []RegisterItem  `json:"register"`
	Settings ItemStatus      `json:"settings"`
	Projects []ProjectItem   `json:"projects"`
	Switch   ItemStatus      `json:"switch"`
	Rollback *RollbackRecord `json:"rollback,omitempty"`

	// StopRecheckAt is the time of a co-located apply's drained re-check
	// immediately before the server stop (the stopped phase's time bound).
	// Nil on the remote path.
	StopRecheckAt *time.Time `json:"stopRecheckAt,omitempty"`
	// SingleHubReplica records the operator's --single-hub-replica
	// assertion on a co-located stopped phase against an external Hub
	// database.
	SingleHubReplica bool `json:"singleHubReplica,omitempty"`
	// HostMode is the host mode this conversion runs in ("colocated" or
	// "remote"), fixed by its first apply: detected from the saved daemon
	// launch, or asserted with --host-mode when detection cannot decide.
	// Every later invocation must agree with it. Empty in a journal
	// written before the field existed.
	HostMode string `json:"hostMode,omitempty"`
	// HostModeSource is "detected" (from the saved daemon launch) or
	// "asserted" (the operator's --host-mode).
	HostModeSource string `json:"hostModeSource,omitempty"`
}

// NewJournal returns a journal for a new run, with a fresh run ID.
func NewJournal(planDigest, legacyBrokerID string, now time.Time) (*Journal, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, fmt.Errorf("generating the convert run ID: %w", err)
	}
	return &Journal{
		SchemaVersion:  JournalSchemaVersion,
		RunID:          hex.EncodeToString(b[:]),
		PlanDigest:     planDigest,
		LegacyBrokerID: legacyBrokerID,
		State:          JournalApplying,
		CreatedAt:      now.UTC(),
		UpdatedAt:      now.UTC(),
		Register:       []RegisterItem{},
		Settings:       ItemStatus{State: StepPending},
		Projects:       []ProjectItem{},
		Switch:         ItemStatus{State: StepPending},
	}, nil
}

// HasStarted reports whether any item is "started": a run was interrupted
// in the middle of an effect.
func (j *Journal) HasStarted() bool {
	if j == nil {
		return false
	}
	if j.Settings.State == StepStarted || j.Switch.State == StepStarted {
		return true
	}
	for _, it := range j.Register {
		if it.State == StepStarted || it.Capacity.State == StepStarted {
			return true
		}
	}
	for _, it := range j.Projects {
		if it.State == StepStarted {
			return true
		}
	}
	if r := j.Rollback; r != nil {
		if r.Settings.State == StepStarted {
			return true
		}
		for _, it := range r.Projects {
			if it.State == StepStarted {
				return true
			}
		}
		for _, it := range r.Deregister {
			if it.State == StepStarted {
				return true
			}
		}
		for _, it := range r.Moved {
			if it.State == StepStarted {
				return true
			}
		}
	}
	return false
}

// ErrJournalUnreadable is returned (wrapped) when a journal exists but
// cannot be used: a link, not owner-only, not a regular file, not valid
// JSON or of another schema version.
var ErrJournalUnreadable = errors.New("the convert journal cannot be read")

// openConvertTree opens the private tree at globalDir and checks that
// runtime-brokers/ and runtime-brokers/.convert/ are real directories, not
// links. With create, missing directories are created owner-only. Without
// it, a missing directory returns an error wrapping fs.ErrNotExist.
func openConvertTree(globalDir string, create bool) (*privatefs.Tree, error) {
	tree, err := privatefs.OpenTree(globalDir, privatefs.TreeOptions{})
	if err != nil {
		return nil, err
	}
	// One level at a time, each checked before anything is created below
	// it, so a link at runtime-brokers/ never has a directory created
	// through it.
	for _, rel := range []string{brokeridentity.RuntimeBrokersDirName, convertRel} {
		d, err := tree.OpenDir(rel)
		if errors.Is(err, fs.ErrNotExist) && create {
			if err := tree.MkdirAll(rel, privatefs.Private); err != nil {
				return nil, err
			}
			d, err = tree.OpenDir(rel)
		}
		if err != nil {
			return nil, err
		}
		_ = d.Close()
	}
	return tree, nil
}

// LoadJournal reads the journal under globalDir. It returns nil and no
// error when there is none. A journal that exists but cannot be read
// privately (a link at it or at a directory above it in the tree, not
// owner-only, not valid) is an error wrapping ErrJournalUnreadable.
func LoadJournal(globalDir string) (*Journal, error) {
	tree, err := openConvertTree(globalDir, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJournalUnreadable, err)
	}
	data, err := tree.ReadPrivate(convertRel+"/"+JournalFileName, maxJournalBytes)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrJournalUnreadable, err)
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("%w: it is not valid JSON", ErrJournalUnreadable)
	}
	if j.SchemaVersion != JournalSchemaVersion {
		return nil, fmt.Errorf("%w: schema version %d, expected %d", ErrJournalUnreadable, j.SchemaVersion, JournalSchemaVersion)
	}
	return &j, nil
}

// SaveJournal atomically replaces the journal under globalDir with j,
// setting j.UpdatedAt to now. The new file is created owner-only in the
// convert directory, synced and renamed over the journal; a link at the
// journal path is replaced, never written through.
func SaveJournal(globalDir string, j *Journal, now time.Time) error {
	if j == nil {
		return errors.New("SaveJournal: nil journal")
	}
	j.SchemaVersion = JournalSchemaVersion
	j.UpdatedAt = now.UTC()
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tree, err := openConvertTree(globalDir, true)
	if err != nil {
		return fmt.Errorf("opening the convert directory: %w", err)
	}
	return replacePrivate(tree, convertRel, JournalFileName, append(data, '\n'))
}

// WriteEvidenceCopy keeps data, the pre-apply settings file, as the
// evidence copy settings-before-<runID>.yaml in the convert directory and
// returns its file name. An existing copy for the run is kept and never
// overwritten.
func WriteEvidenceCopy(globalDir, runID string, data []byte) (string, error) {
	return writeRunCopy(globalDir, EvidenceFilePrefix+runID+".yaml", data)
}

// WriteInstancesCopy keeps data, the instances list step 2 writes as YAML,
// as instances-written-<runID>.yaml in the convert directory and returns
// its file name. An existing copy for the run is kept.
func WriteInstancesCopy(globalDir, runID string, data []byte) (string, error) {
	return writeRunCopy(globalDir, InstancesFilePrefix+runID+".yaml", data)
}

// ReplaceInstancesCopy replaces the instances copy name with data, owner-
// only and atomically, so it equals what step 2 writes even when a resumed
// run carries a changed list (the plan digest leaves env values out). The
// evidence copy is never replaced.
func ReplaceInstancesCopy(globalDir, name string, data []byte) error {
	if !validRunCopyName(name) || !strings.HasPrefix(name, InstancesFilePrefix) {
		return fmt.Errorf("%w: invalid file name", ErrJournalUnreadable)
	}
	tree, err := openConvertTree(globalDir, true)
	if err != nil {
		return fmt.Errorf("opening the convert directory: %w", err)
	}
	return replacePrivate(tree, convertRel, name, data)
}

// ReadRunCopy reads the owner-only file name from the convert directory.
func ReadRunCopy(globalDir, name string) ([]byte, error) {
	if !validRunCopyName(name) {
		return nil, fmt.Errorf("%w: invalid file name", ErrJournalUnreadable)
	}
	tree, err := openConvertTree(globalDir, false)
	if err != nil {
		return nil, err
	}
	return tree.ReadPrivate(convertRel+"/"+name, maxJournalBytes)
}

// validRunCopyName reports whether name is a plain file name with one of
// the run copy prefixes.
func validRunCopyName(name string) bool {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return false
	}
	return strings.HasPrefix(name, EvidenceFilePrefix) || strings.HasPrefix(name, InstancesFilePrefix)
}

func writeRunCopy(globalDir, name string, data []byte) (string, error) {
	tree, err := openConvertTree(globalDir, true)
	if err != nil {
		return "", fmt.Errorf("opening the convert directory: %w", err)
	}
	f, err := tree.OpenPrivate(convertRel+"/"+name, os.O_RDONLY)
	if err == nil {
		_ = f.Close()
		return name, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := replacePrivate(tree, convertRel, name, data); err != nil {
		return "", err
	}
	return name, nil
}

// replacePrivate writes data to relDir/name through an owner-only
// temporary file in relDir: write, fsync, rename on the handle (which
// checks it is still owner-only), then fsync the directory.
func replacePrivate(tree *privatefs.Tree, relDir, name string, data []byte) error {
	f, tmpRel, err := tree.CreateTemp(relDir, "."+name+".tmp-*", privatefs.Private)
	if err != nil {
		return err
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = tree.Remove(tmpRel)
		}
		_ = f.Close()
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := tree.RenameOpen(f, relDir+"/"+name); err != nil {
		return err
	}
	renamed = true
	return syncTreeDir(tree, relDir)
}

// syncTreeDir fsyncs the directory rel, so a rename in it is durable.
func syncTreeDir(tree *privatefs.Tree, rel string) error {
	d, err := tree.OpenDir(rel)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
