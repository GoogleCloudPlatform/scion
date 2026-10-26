//go:build !unix

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
	"errors"
	"fmt"
)

// execRunCommand always fails with errors.ErrUnsupported. NFS mounts are
// made only by a Runtime Broker, which is not supported on this platform,
// and the unix version's process-group kill on timeout has no equivalent
// here. Nothing is run.
func execRunCommand(_ context.Context, name string, _ ...string) ([]byte, error) {
	return nil, fmt.Errorf("run %s: %w on this platform", name, errors.ErrUnsupported)
}
