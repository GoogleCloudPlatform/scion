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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	yamlv3 "gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// Rollback undoes an apply before retirement (design sections 8.1 and
// 8.2), for the items the journal marks started or done. It holds the
// host lock, needs the server stopped, and refuses while a flat instance
// has a non-terminal agent. Each step checks live state first, so a
// re-run after a failure continues where it stopped.
func Rollback(ctx context.Context, env *Env) (*Report, error) {
	lock, err := AcquireHostLock(env.GlobalDir)
	if err != nil {
		if errors.Is(err, ErrHostLockHeld) {
			return refused(ModeRollback, nil, refuse(RefuseLockHeld))
		}
		return refused(ModeRollback, nil, refuse(RefuseLocalWrite))
	}
	defer lock.Release()

	r, rep, err := startRollback(ctx, env)
	if rep != nil {
		return rep, err
	}
	// The server must be stopped (the offline window holds).
	if env.ServerRunning != nil && env.ServerRunning(ctx) {
		return refused(ModeRollback, r.j, refuse(RefuseServerRunning))
	}
	notes, ref := r.rollbackHub()
	if ref != nil {
		return refused(ModeRollback, r.j, ref)
	}
	if ref := r.rollbackLocal(); ref != nil {
		return refused(ModeRollback, r.j, ref)
	}
	rep = reportFrom(ModeRollback, r.j)
	rep.Notes = append(rep.Notes, notes...)
	rep.Notes = append(rep.Notes, NoteRolledBack)
	return rep, nil
}

// startRollback loads the journal of the run to roll back. A non-nil
// report ends the rollback with a refusal, also returned as the error.
func startRollback(ctx context.Context, env *Env) (*run, *Report, error) {
	j, err := LoadJournal(env.GlobalDir)
	if err != nil {
		rep, err := refused(ModeRollback, nil, refuse(RefuseLocalWrite))
		return nil, rep, err
	}
	if j == nil {
		rep, err := refused(ModeRollback, nil, refuse(RefuseNoJournal))
		return nil, rep, err
	}
	if j.State == JournalRolledBack {
		rep, err := refused(ModeRollback, j, refuse(RefuseNothingToRollBack))
		return nil, rep, err
	}
	return &run{ctx: ctx, env: env, j: j}, nil, nil
}

// rollbackHub is the Hub part of a rollback: section 8.1 steps 0 to 3.
// It returns the notes of step 0 for the report.
func (r *run) rollbackHub() ([]string, *Refusal) {
	j := r.j
	// 0. A registration that started but recorded no Runtime Broker ID
	// (it failed, or the run was cut short, after the Hub may have created
	// the row) is named from the instance identity file, so that row is
	// deregistered before the directory moves.
	notes, ref := r.fillRegisteredIDs()
	if ref != nil {
		return nil, ref
	}
	// 1. No flat instance may have a non-terminal agent.
	for _, it := range j.Register {
		if it.BrokerID == "" {
			continue
		}
		if ref := r.drained(it.BrokerID, RefuseInstanceDrained); ref != nil {
			return nil, ref
		}
	}

	if j.Rollback == nil {
		j.Rollback = newRollbackRecord(j)
	}
	ensureDeregisterItems(j)
	j.State = JournalRollingBack
	if ref := r.save(); ref != nil {
		return nil, ref
	}

	// 2. Per project: restore the default, remove the instance links.
	if ref := r.rollbackProjects(); ref != nil {
		return nil, ref
	}
	// 3. Deregister each instance row (its secret goes with it).
	for i := range j.Rollback.Deregister {
		if ref := r.deregister(&j.Rollback.Deregister[i]); ref != nil {
			return nil, ref
		}
	}
	return notes, nil
}

// rollbackLocal is the local part of a rollback: section 8.1 steps 4 to 6.
func (r *run) rollbackLocal() *Refusal {
	j := r.j
	// 4. Remove the instances key, only while it equals what apply wrote.
	if ref := r.removeSettings(); ref != nil {
		return ref
	}
	// 5. Move each identity directory aside within the tree.
	for i := range j.Rollback.Moved {
		if ref := r.moveAside(&j.Rollback.Moved[i]); ref != nil {
			return ref
		}
	}
	// 6. Done.
	j.State = JournalRolledBack
	return r.save()
}

// fillRegisteredIDs records, for each register item that is not pending
// and has no Runtime Broker ID, the ID its identity file carries. It
// returns a note (the key only) for each such item whose identity file is
// absent or records no ID, so the operator learns that no row could be
// named for it.
func (r *run) fillRegisteredIDs() ([]string, *Refusal) {
	var notes []string
	changed := false
	for i := range r.j.Register {
		it := &r.j.Register[i]
		if it.State == StepPending || it.BrokerID != "" {
			continue
		}
		id, ok, err := identityBrokerID(r.env.GlobalDir, it.Key)
		if err != nil {
			ref := refuse(RefuseIdentityUnreadable)
			ref.Keys = []string{it.Key}
			return nil, ref
		}
		if !ok {
			notes = append(notes, NoteNoIdentity+" "+it.Key)
			continue
		}
		it.BrokerID = id
		changed = true
	}
	if changed {
		if ref := r.save(); ref != nil {
			return nil, ref
		}
	}
	return notes, nil
}

// ensureDeregisterItems adds a deregister item for each register item
// that is not pending and names a Runtime Broker ID the rollback record
// does not list yet (an ID filled from an identity file on a resumed
// rollback).
func ensureDeregisterItems(j *Journal) {
	listed := map[string]bool{}
	for _, d := range j.Rollback.Deregister {
		listed[d.BrokerID] = true
	}
	for _, it := range j.Register {
		if it.State == StepPending || it.BrokerID == "" || listed[it.BrokerID] {
			continue
		}
		j.Rollback.Deregister = append(j.Rollback.Deregister, DeregisterItem{BrokerID: it.BrokerID, ItemStatus: ItemStatus{State: StepPending}})
		listed[it.BrokerID] = true
	}
}

// newRollbackRecord lists the rollback items for the apply items the
// journal marks started or done (section 8.2).
func newRollbackRecord(j *Journal) *RollbackRecord {
	rb := &RollbackRecord{
		Projects:   []RollbackProjectItem{},
		Deregister: []DeregisterItem{},
		Moved:      []MovedItem{},
		Settings:   ItemStatus{State: StepPending},
	}
	for _, p := range j.Projects {
		if p.State != StepPending {
			rb.Projects = append(rb.Projects, RollbackProjectItem{ProjectID: p.ProjectID, ItemStatus: ItemStatus{State: StepPending}})
		}
	}
	for _, it := range j.Register {
		if it.State == StepPending {
			continue
		}
		if it.BrokerID != "" {
			rb.Deregister = append(rb.Deregister, DeregisterItem{BrokerID: it.BrokerID, ItemStatus: ItemStatus{State: StepPending}})
		}
		rb.Moved = append(rb.Moved, MovedItem{Key: it.Key, ItemStatus: ItemStatus{State: StepPending}})
	}
	if j.Settings.State == StepPending {
		rb.Settings.State = StepDone // nothing was written
	}
	return rb
}

func isNotFound(err error) bool { return apiclient.IsNotFoundError(err) }

// instanceKeyRe is the frozen instance key shape; a key that does not
// match is never used as a path component.
var instanceKeyRe = regexp.MustCompile(config.RuntimeBrokerInstanceKeyPattern)

func (r *run) rollbackProjects() *Refusal {
	applied := map[string]ProjectItem{}
	for _, p := range r.j.Projects {
		applied[p.ProjectID] = p
	}
	instances := map[string]bool{}
	for _, it := range r.j.Register {
		if it.BrokerID != "" {
			instances[it.BrokerID] = true
		}
	}
	c := r.env.Client.Projects()
	for i := range r.j.Rollback.Projects {
		rp := &r.j.Rollback.Projects[i]
		if rp.State == StepDone {
			continue
		}
		ap := applied[rp.ProjectID]
		rp.ItemStatus = ItemStatus{State: StepStarted}
		if ref := r.save(); ref != nil {
			return ref
		}
		proj, err := c.Get(r.ctx, rp.ProjectID)
		if err != nil && !isNotFound(err) {
			return r.failRollbackProject(rp, refuseHub(RefuseHubRequest, err))
		}
		if err == nil && proj != nil && ap.OldDefault == r.j.LegacyBrokerID && instances[proj.DefaultRuntimeBrokerID] {
			if _, err := c.Update(r.ctx, rp.ProjectID, &hubclient.UpdateProjectRequest{DefaultRuntimeBrokerID: r.j.LegacyBrokerID}); err != nil {
				return r.failRollbackProject(rp, refuseHub(RefuseHubRequest, err))
			}
		}
		if err == nil && proj != nil {
			for _, id := range ap.Linked {
				if err := c.RemoveProvider(r.ctx, rp.ProjectID, id); err != nil && !isNotFound(err) {
					return r.failRollbackProject(rp, refuseHub(RefuseHubRequest, err))
				}
			}
		}
		rp.ItemStatus = ItemStatus{State: StepDone}
		if ref := r.save(); ref != nil {
			return ref
		}
	}
	return nil
}

func (r *run) failRollbackProject(rp *RollbackProjectItem, ref *Refusal) *Refusal {
	rp.Error = fixedError(ref)
	ref.Projects = []string{rp.ProjectID}
	if sref := r.save(); sref != nil {
		return sref
	}
	return ref
}

func (r *run) deregister(it *DeregisterItem) *Refusal {
	if it.State == StepDone {
		return nil
	}
	it.ItemStatus = ItemStatus{State: StepStarted}
	if ref := r.save(); ref != nil {
		return ref
	}
	if err := r.env.Client.RuntimeBrokers().Delete(r.ctx, it.BrokerID); err != nil && !isNotFound(err) {
		ref := refuseHub(RefuseHubRequest, err)
		it.Error = fixedError(ref)
		if sref := r.save(); sref != nil {
			return sref
		}
		return ref
	}
	it.ItemStatus = ItemStatus{State: StepDone}
	return r.save()
}

func (r *run) removeSettings() *Refusal {
	rb := r.j.Rollback
	if rb.Settings.State == StepDone {
		return nil
	}
	if r.j.InstancesFile == "" {
		return refuse(RefuseSettingsChanged)
	}
	data, err := ReadRunCopy(r.env.GlobalDir, r.j.InstancesFile)
	if err != nil {
		return refuse(RefuseLocalWrite)
	}
	var expected []config.V1RuntimeBrokerInstanceConfig
	if err := yamlv3.Unmarshal(data, &expected); err != nil {
		return refuse(RefuseLocalWrite)
	}
	rb.Settings = ItemStatus{State: StepStarted}
	if ref := r.save(); ref != nil {
		return ref
	}
	if err := r.env.RemoveInstances(expected); err != nil {
		var ref *Refusal
		var mm *config.InstancesMismatchError
		if errors.As(err, &mm) {
			ref = refuse(RefuseSettingsChanged)
			ref.Keys = mm.Keys
		} else {
			ref = refuse(RefuseSettingsWrite)
		}
		rb.Settings.Error = fixedError(ref)
		if sref := r.save(); sref != nil {
			return sref
		}
		return ref
	}
	rb.Settings = ItemStatus{State: StepDone}
	return r.save()
}

// moveAside renames runtime-brokers/<key> to
// runtime-brokers/.convert/rolled-back/<runId>/<key>: a rename within the
// tree, so its owner-only modes are kept. A link at the source or at any
// directory on the destination side is refused, and an existing
// destination is never replaced. The directory is never deleted.
func (r *run) moveAside(it *MovedItem) *Refusal {
	if it.State == StepDone {
		return nil
	}
	// The row must be gone first, so the credentials inside are dead: a
	// key that names a Runtime Broker ID moves only once that ID's
	// deregister item is done.
	for _, reg := range r.j.Register {
		if reg.Key != it.Key || reg.BrokerID == "" {
			continue
		}
		deregistered := false
		for _, d := range r.j.Rollback.Deregister {
			if d.BrokerID == reg.BrokerID && d.State == StepDone {
				deregistered = true
			}
		}
		if !deregistered {
			return refuse(RefuseHubRequest)
		}
	}
	if r.env.BeforeMove != nil {
		if err := r.env.BeforeMove(r.env.GlobalDir, it.Key); err != nil {
			ref := refuse(RefuseLocalWrite)
			it.Error = fixedError(ref)
			if sref := r.save(); sref != nil {
				return sref
			}
			return ref
		}
	}
	it.ItemStatus = ItemStatus{State: StepStarted}
	if ref := r.save(); ref != nil {
		return ref
	}
	if err := moveIdentityDir(r.env.GlobalDir, r.j.RunID, it.Key); err != nil {
		ref := refuse(RefuseLocalWrite)
		it.Error = fixedError(ref)
		if sref := r.save(); sref != nil {
			return sref
		}
		return ref
	}
	it.ItemStatus = ItemStatus{State: StepDone}
	return r.save()
}

// moveIdentityDir moves the identity directory of key aside for run. A
// missing source (already moved, or never created) is done.
func moveIdentityDir(globalDir, runID, key string) error {
	if !instanceKeyRe.MatchString(key) {
		return fmt.Errorf("invalid instance key")
	}
	tree, err := openConvertTree(globalDir, true)
	if err != nil {
		return err
	}
	src := brokeridentity.RuntimeBrokersDirName + "/" + key
	d, err := tree.OpenDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_ = d.Close()
	for _, rel := range []string{convertRel + "/" + RolledBackDirName, convertRel + "/" + RolledBackDirName + "/" + runID} {
		d, err := tree.OpenDir(rel)
		if errors.Is(err, fs.ErrNotExist) {
			if err := tree.MkdirAll(rel, privatefs.Private); err != nil {
				return err
			}
			d, err = tree.OpenDir(rel)
		}
		if err != nil {
			return err
		}
		_ = d.Close()
	}
	dst := convertRel + "/" + RolledBackDirName + "/" + runID + "/" + key
	if _, err := os.Lstat(filepath.Join(globalDir, filepath.FromSlash(dst))); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("the moved-aside directory for %s already exists", key)
	}
	return tree.Rename(src, dst)
}
