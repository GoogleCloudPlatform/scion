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

	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// Test-only lifecycle hooks (ptone/scion#5878): called only from pkg/hub
// tests, so they live in a _test.go file.

// errLifecycleUnsupported reports a store without lifecycle support.
var errLifecycleUnsupported = errors.New("lifecycle events: the store does not support the lifecycle observer")

// installLifecycleObserver is the test hook that installs the observer
// whatever the experiment says (the slice 1 hook). The experiment reconcile
// leaves an observer installed this way alone. It returns the resulting
// state.
func (s *Server) installLifecycleObserver() (string, error) {
	ls, ok := s.store.(lifecycleEventStore)
	if !ok {
		return "", errLifecycleUnsupported
	}
	rt := &s.lifecycleEvents
	rt.mu.Lock()
	defer rt.mu.Unlock()
	state, err := s.installObserverLocked(ls)
	if err != nil {
		return "", err
	}
	rt.testInstalled = state == telemetryemit.ObserverStateEnabled
	if rt.started {
		s.ensureDrainerLocked(s.lifecycleContext(context.Background()), ls)
	}
	return state, nil
}
