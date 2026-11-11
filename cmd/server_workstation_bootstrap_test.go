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

package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// fakeLockingStore is a store.Store that also implements store.AdvisoryLocker
// with a fixed outcome. The embedded nil store.Store is never called.
type fakeLockingStore struct {
	store.Store
	acquired bool
	lockErr  error
	keys     []store.AdvisoryLockKey
	released int
}

func (f *fakeLockingStore) TryAdvisoryLock(_ context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	f.keys = append(f.keys, key)
	return f.acquired, func() error { f.released++; return nil }, f.lockErr
}

func (f *fakeLockingStore) TryAdvisoryLockObject(_ context.Context, _ store.AdvisoryLockKey, _ int32) (bool, func() error, error) {
	return f.acquired, func() error { return nil }, nil
}

// Compile-time guard: if AdvisoryLocker grows, this fake must too, or
// runWithAdvisoryLock would silently treat it as a non-locking store.
var _ store.AdvisoryLocker = (*fakeLockingStore)(nil)

type recordingBootstrapper struct {
	templateDirs []string
	hcDirs       []string
}

func (r *recordingBootstrapper) BootstrapTemplatesFromDir(_ context.Context, dir string) error {
	r.templateDirs = append(r.templateDirs, dir)
	return nil
}

func (r *recordingBootstrapper) BootstrapHarnessConfigsFromDir(_ context.Context, dir string) error {
	r.hcDirs = append(r.hcDirs, dir)
	return nil
}

// ptone/scion#1079: when another replica holds the bundled-resources lock,
// neither workstation import runs.
func TestBootstrapWorkstationResources_LockHeldSkipsBootstrap(t *testing.T) {
	s := &fakeLockingStore{acquired: false}
	b := &recordingBootstrapper{}

	bootstrapWorkstationResources(context.Background(), s, b, "/home/u/.scion")

	assert.Equal(t, []store.AdvisoryLockKey{store.LockBundledResources}, s.keys,
		"the workstation bootstrap must take the bundled-resources lock exactly once")
	assert.Empty(t, b.templateDirs, "template bootstrap must not run when the lock is held")
	assert.Empty(t, b.hcDirs, "harness-config bootstrap must not run when the lock is held")
}

// When the lock is acquired, both imports run under that single lock and the
// lock is released afterwards.
func TestBootstrapWorkstationResources_LockAcquiredRunsBoth(t *testing.T) {
	s := &fakeLockingStore{acquired: true}
	b := &recordingBootstrapper{}
	globalDir := "/home/u/.scion"

	bootstrapWorkstationResources(context.Background(), s, b, globalDir)

	assert.Equal(t, []store.AdvisoryLockKey{store.LockBundledResources}, s.keys)
	assert.Equal(t, []string{filepath.Join(globalDir, "templates")}, b.templateDirs)
	assert.Equal(t, []string{filepath.Join(globalDir, "harness-configs")}, b.hcDirs)
	assert.Equal(t, 1, s.released, "the lock must be released after the bootstrap")
}

// A store without an AdvisoryLocker (or nil) runs both imports unconditionally.
func TestBootstrapWorkstationResources_NoLockerRunsBoth(t *testing.T) {
	b := &recordingBootstrapper{}

	bootstrapWorkstationResources(context.Background(), nil, b, "/g")

	assert.Len(t, b.templateDirs, 1)
	assert.Len(t, b.hcDirs, 1)
}

// A lock-acquire error skips the import for this boot (documented caveat;
// same behaviour as the hosted branch).
func TestBootstrapWorkstationResources_LockErrorSkipsBootstrap(t *testing.T) {
	s := &fakeLockingStore{lockErr: errors.New("pool exhausted")}
	b := &recordingBootstrapper{}

	bootstrapWorkstationResources(context.Background(), s, b, "/g")

	assert.Equal(t, []store.AdvisoryLockKey{store.LockBundledResources}, s.keys)
	assert.Empty(t, b.templateDirs)
	assert.Empty(t, b.hcDirs)
}
