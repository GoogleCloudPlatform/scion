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

package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// ResourceHandle identifies one runtime resource created during a launch
// (design t1-async-create-v11.md §3.8.4), reported by the runtime via
// StartOptions.OnResourceCreated after each true create. It is defined in
// pkg/api so pkg/runtime can produce it without importing pkg/agent.
type ResourceHandle = api.ResourceHandle

// UIDPreconditionDeleter is an optional capability a runtime.Runtime may
// implement: delete one named resource only if its current UID still
// matches the UID recorded when it was created (design §3.8.4), so a delete
// that overlaps a newer launch's recreate of the same name can never remove
// the newer launch's resource. The Kubernetes runtime implements it for
// secrets, SecretProviderClasses and pods, and the Docker-family runtimes
// by container ID. CleanupLaunch skips (and reports an error for)
// any handle whose runtime does not implement this, rather than deleting it
// unconditionally by name.
type UIDPreconditionDeleter interface {
	DeleteResource(ctx context.Context, handle ResourceHandle) error
}

// CleanupLaunch deletes every handle created during an aborted launch
// (design §3.8.4). Callers must pass a fresh context — never a launch's own
// bounded ctx' — because cleanup must still run after that context has
// expired or been cancelled; the broker's runLaunch constructs a dedicated
// context.WithTimeout(context.Background(), 60*time.Second) for this call.
// Every handle is attempted even if one fails; the returned error joins all
// failures (nil if every delete succeeded).
func (m *AgentManager) CleanupLaunch(ctx context.Context, handles []ResourceHandle) error {
	deleter, supportsUIDPrecondition := m.Runtime.(UIDPreconditionDeleter)

	var errs []error
	for _, h := range handles {
		if !supportsUIDPrecondition {
			// A plain Delete(ctx, h.Name) has no UID check, which is exactly
			// what the precondition exists to prevent (a stale launch's
			// cleanup deleting a newer launch's same-named resource), and
			// once a handle can name something other than the agent itself
			// (a Kubernetes Secret, say), Delete(name) would wrongly treat
			// that name as an agent/container ID. Skip the handle instead of
			// guessing.
			slog.Warn("CleanupLaunch: runtime has no UID-precondition delete; skipping handle rather than deleting unconditionally by name",
				"kind", h.Kind, "namespace", h.Namespace, "name", h.Name)
			errs = append(errs, fmt.Errorf("cleanup launch resource %s %s/%s: runtime %s does not support UID-precondition delete", h.Kind, h.Namespace, h.Name, m.Runtime.Name()))
			continue
		}
		if err := deleter.DeleteResource(ctx, h); err != nil {
			errs = append(errs, fmt.Errorf("cleanup launch resource %s %s/%s: %w", h.Kind, h.Namespace, h.Name, err))
		}
	}
	return errors.Join(errs...)
}
