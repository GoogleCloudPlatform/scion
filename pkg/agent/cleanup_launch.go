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
)

// ResourceHandle identifies one runtime resource created during a launch
// (design t1-async-create-v11.md §3.8.4), reported by the runtime via
// opts.OnResourceCreated (wired in P1b-2) after each true create. Namespace
// and UID are empty for runtimes that have neither concept (e.g. a Docker
// container has an ID but no namespace).
type ResourceHandle struct {
	Kind      string // e.g. "secret", "secretproviderclass", "pod", "container"
	Namespace string
	Name      string
	UID       string
}

// UIDPreconditionDeleter is an optional capability a runtime.Runtime may
// implement: delete one named resource only if its current UID still
// matches the UID recorded when it was created (design §3.8.4), so a delete
// racing a newer launch's recreate of the same name can never remove the
// newer launch's resource. P1b-2 adds this to the Kubernetes runtime (for
// secrets, SecretProviderClasses and pods) and the Docker-family runtimes
// (by container ID). A runtime that does not implement it falls back to a
// plain delete-by-name in CleanupLaunch below, which is today's synchronous-
// create cleanup behavior and therefore safe for this inert phase, where no
// runtime yet calls OnResourceCreated and handles is always empty in
// practice.
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
		var err error
		if supportsUIDPrecondition {
			err = deleter.DeleteResource(ctx, h)
		} else {
			err = m.Runtime.Delete(ctx, h.Name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("cleanup launch resource %s %s/%s: %w", h.Kind, h.Namespace, h.Name, err))
		}
	}
	return errors.Join(errs...)
}
