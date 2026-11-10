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

package brokeridentity

import (
	"errors"
	"fmt"
)

// lockDir always fails with errors.ErrUnsupported. The unix version takes a
// flock on <dir>/.lock; the broker identity is used only by a Runtime
// Broker, which is not supported on this platform. It creates nothing.
func lockDir(dir string) (func(), error) {
	return nil, fmt.Errorf("lock instance directory %s: %w on this platform", dir, errors.ErrUnsupported)
}
