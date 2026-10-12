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

package hubtestutil

import (
	"sync/atomic"
)

// storeFaultSwitch gates a store wrapper's failure injection. Wrappers
// delegate to the inner store until Arm is called. A nil *storeFaultSwitch
// is always active, so a wrapper built without a switch keeps failing
// unconditionally (the pre-switch behavior).
type StoreFaultSwitch struct {
	armed atomic.Bool
}

// Arm turns on failure injection for every wrapper sharing this switch. It
// is safe to call from any goroutine.
func (f *StoreFaultSwitch) Arm() { f.armed.Store(true) }

// Active reports whether a wrapper should inject its failure now.
func (f *StoreFaultSwitch) Active() bool { return f == nil || f.armed.Load() }
