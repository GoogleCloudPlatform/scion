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

//go:build !unix

package telemetry

import (
	"errors"
	"fmt"
)

// The outbox serializes every mutation with flock(2), which this platform
// lacks. The outbox belongs to sciontool, which runs inside the (Linux)
// agent container, so on this platform every locked operation fails with
// errors.ErrUnsupported. See analytics_outbox_unix.go.

// lock always fails with errors.ErrUnsupported on this platform.
func (o *AnalyticsOutbox) lock() (func(), error) {
	return nil, fmt.Errorf("analytics outbox: locking: %w on this platform", errors.ErrUnsupported)
}

// linksUnsupported reports false for every error on this platform, so
// linkNoReplace never takes its rename fallback and returns the link error.
func linksUnsupported(err error) bool {
	return false
}
