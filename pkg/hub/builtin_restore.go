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

package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/resources"
)

// Restore of deleted built-in resources (ptone/scion#3544, Phase 2).
//
// Startup bootstrap no longer re-creates a built-in harness config or
// template that was deleted (the seeded-built-ins ledger remembers it). The
// explicit restore path below is the way to get one back: it re-creates the
// global row from this binary's embedded catalog.

// ErrNotBuiltin is returned by RestoreBuiltin for a name that is not a
// built-in resource of the requested kind.
var ErrNotBuiltin = errors.New("not a built-in resource")

// errBuiltinRestoreBusy is returned when the bundled-resources advisory lock
// stays held by another replica (normally a bootstrap in progress) for the
// whole wait.
var errBuiltinRestoreBusy = errors.New("bundled resources are being bootstrapped by another replica; retry shortly")

// builtinRestoreLockWait bounds how long RestoreBuiltin waits for the
// bundled-resources advisory lock. Bootstrap holds it only briefly at boot.
var builtinRestoreLockWait = 10 * time.Second

// builtinRestoreLockPoll is the retry interval while waiting for the lock.
const builtinRestoreLockPoll = 200 * time.Millisecond

// builtinSeedLedgerRestoredBy is the updated_by value restore writes.
const builtinSeedLedgerRestoredBy = "restore"

// builtinCatalogEntry returns the bundled resource of the given kind whose
// slug is name.
func builtinCatalogEntry(kind storage.ResourceKind, name string) (resources.BundledResource, bool) {
	var entries []resources.BundledResource
	switch kind {
	case storage.ResourceKindHarnessConfig:
		entries = resources.BuiltinHarnessConfigs()
	case storage.ResourceKindTemplate:
		entries = resources.BuiltinTemplates()
	}
	for _, r := range entries {
		if api.Slugify(r.Name) == name {
			return r, true
		}
	}
	return resources.BundledResource{}, false
}

// builtinNames returns the sorted built-in slugs of the given kind.
func builtinNames(kind storage.ResourceKind) []string {
	var names []string
	switch kind {
	case storage.ResourceKindHarnessConfig:
		for _, n := range resources.BuiltinHarnessConfigNames() {
			names = append(names, api.Slugify(n))
		}
	case storage.ResourceKindTemplate:
		for _, r := range resources.BuiltinTemplates() {
			names = append(names, api.Slugify(r.Name))
		}
	}
	sort.Strings(names)
	return names
}

// acquireBundledResourcesLock takes store.LockBundledResources, the lock both
// startup bootstraps hold, retrying for up to builtinRestoreLockWait. A store
// without advisory locks (or SQLite, where the lock always succeeds) runs
// unlocked; s.builtinRestoreMu still serializes restores in this process.
func (s *Server) acquireBundledResourcesLock(ctx context.Context) (func(), error) {
	locker, ok := s.store.(store.AdvisoryLocker)
	if !ok {
		return func() {}, nil
	}
	deadline := time.Now().Add(builtinRestoreLockWait)
	for {
		acquired, release, err := locker.TryAdvisoryLock(ctx, store.LockBundledResources)
		if err != nil {
			return nil, fmt.Errorf("acquire bundled resources lock: %w", err)
		}
		if acquired {
			return func() {
				if err := release(); err != nil {
					s.resourceLog.Warn("failed to release bundled resources lock", "error", err)
				}
			}, nil
		}
		_ = release()
		if time.Now().After(deadline) {
			return nil, errBuiltinRestoreBusy
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(builtinRestoreLockPoll):
		}
	}
}

// RestoreBuiltin re-creates the global row of a deleted built-in harness
// config or template from this binary's embedded catalog.
//
//   - A name that is not a built-in of kind returns ErrNotBuiltin.
//   - If a global row with that slug already exists (any status), nothing
//     changes and created is false. Resetting an existing row's content is
//     the job of reimport/reset, not restore.
//   - Otherwise the row is created with the same embed-sourced
//     BootstrapSource call startup bootstrap uses, under
//     LockBundledResources, and the name is marked in the seeded-built-ins
//     ledger. Restore never removes a name from the ledger.
//
// Restore runs outside the startup bootstrap, so the ledger save relies on
// its CAS merge loop rather than on the lock alone.
func (s *Server) RestoreBuiltin(ctx context.Context, kind storage.ResourceKind, name string) (created bool, err error) {
	entry, ok := builtinCatalogEntry(kind, name)
	if !ok {
		return false, fmt.Errorf("%w: %s %q", ErrNotBuiltin, kind, name)
	}

	s.builtinRestoreMu.Lock()
	defer s.builtinRestoreMu.Unlock()

	release, err := s.acquireBundledResourcesLock(ctx)
	if err != nil {
		return false, err
	}
	defer release()

	exists, err := s.builtinRowExists(ctx, kind, name)
	if err != nil {
		return false, fmt.Errorf("restore %s %q: %w", kind, name, err)
	}
	if exists {
		return false, nil
	}

	// Load the ledger before creating anything so an unreadable ledger
	// fails the restore instead of leaving a row it cannot record.
	ledger, err := s.loadBuiltinSeedLedger(ctx)
	if err != nil {
		return false, fmt.Errorf("restore %s %q: %w", kind, name, err)
	}

	var rs *ResourceStore
	switch kind {
	case storage.ResourceKindTemplate:
		rs = s.templateStore()
	case storage.ResourceKindHarnessConfig:
		harness, err := resolveHarnessType(entry)
		if err != nil {
			return false, fmt.Errorf("restore %s %q: %w", kind, name, err)
		}
		rs = s.harnessConfigStore(harness)
	default:
		return false, fmt.Errorf("restore: unsupported resource kind %q", kind)
	}

	result, err := rs.BootstrapSource(ctx, NewFSResourceSource(entry), BootstrapOptions{})
	if err != nil {
		return false, fmt.Errorf("restore %s %q: %w", kind, name, err)
	}
	if result.Created == 0 {
		return false, fmt.Errorf("restore %s %q: row was not created", kind, name)
	}

	ledger.Mark(kind, name)
	ledger.updatedBy = builtinSeedLedgerRestoredBy
	if err := s.saveBuiltinSeedLedger(ctx, ledger); err != nil {
		// The row exists; a ledger that misses the name only means the
		// next bootstrap marks it from the present row.
		s.resourceLog.Warn("restored built-in but could not update the seeded ledger",
			"kind", kind, "name", name, "error", err)
	}

	s.resourceLog.Info("restored built-in resource", "kind", kind, "name", name)
	return true, nil
}

// RestoreBuiltinsRequest is the body of POST /api/v1/harness-configs/restore
// and POST /api/v1/templates/restore. Exactly one of Names or All is set.
type RestoreBuiltinsRequest struct {
	Names []string `json:"names,omitempty"`
	All   bool     `json:"all,omitempty"`
}

// RestoreBuiltinsResponse lists which requested built-ins were re-created
// and which already had a global row.
type RestoreBuiltinsResponse struct {
	Restored       []string `json:"restored"`
	AlreadyPresent []string `json:"alreadyPresent"`
}

// handleBuiltinRestore serves the kind-specific restore routes. Authz is
// global ActionCreate on the resource kind, the same check as a global
// create. It answers 201 when at least one row was re-created, else 200.
func (s *Server) handleBuiltinRestore(w http.ResponseWriter, r *http.Request, kind storage.ResourceKind) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	// SECURITY-GATE: global create on this resource kind, before any read of
	// the body or any mutation.
	var res Resource
	switch kind {
	case storage.ResourceKindHarnessConfig:
		res = harnessConfigScopeResource(store.HarnessConfigScopeGlobal, "")
	case storage.ResourceKindTemplate:
		res = templateScopeResource(store.TemplateScopeGlobal, "")
	default:
		NotFound(w, "Resource")
		return
	}
	if !s.authorize(w, r, res, ActionCreate) {
		return
	}

	var req RestoreBuiltinsRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	var names []string
	switch {
	case req.All && len(req.Names) > 0:
		ValidationError(w, "specify either names or all, not both", nil)
		return
	case req.All:
		names = builtinNames(kind)
	case len(req.Names) == 0:
		ValidationError(w, "names or all is required", nil)
		return
	default:
		seen := map[string]bool{}
		for _, n := range req.Names {
			n = strings.TrimSpace(n)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			names = append(names, n)
		}
		if len(names) == 0 {
			ValidationError(w, "names or all is required", nil)
			return
		}
	}

	// Reject the whole request if any name is not a built-in, before any
	// mutation.
	var invalid []string
	for _, n := range names {
		if _, ok := builtinCatalogEntry(kind, n); !ok {
			invalid = append(invalid, n)
		}
	}
	if len(invalid) > 0 {
		ValidationError(w, fmt.Sprintf("not a built-in %s: %s", kind, strings.Join(invalid, ", ")),
			map[string]interface{}{"invalid": invalid, "builtins": builtinNames(kind)})
		return
	}

	resp := RestoreBuiltinsResponse{Restored: []string{}, AlreadyPresent: []string{}}
	for _, n := range names {
		created, err := s.RestoreBuiltin(r.Context(), kind, n)
		switch {
		case errors.Is(err, errBuiltinRestoreBusy):
			ServiceNotReady(w, err.Error())
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, err.Error(),
				map[string]interface{}{"restored": resp.Restored, "alreadyPresent": resp.AlreadyPresent})
			return
		case created:
			resp.Restored = append(resp.Restored, n)
		default:
			resp.AlreadyPresent = append(resp.AlreadyPresent, n)
		}
	}

	status := http.StatusOK
	if len(resp.Restored) > 0 {
		status = http.StatusCreated
	}
	writeJSON(w, status, resp)
}
