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

package settingslockhook

import (
	"testing"
	"time"
)

func TestSetAndRestore(t *testing.T) {
	if got := Wait(10 * time.Second); got != 10*time.Second {
		t.Fatalf("default = %s", got)
	}
	restore := Set(300 * time.Millisecond)
	if got := Wait(10 * time.Second); got != 300*time.Millisecond {
		t.Errorf("override = %s", got)
	}
	inner := Set(time.Second)
	if got := Wait(10 * time.Second); got != time.Second {
		t.Errorf("nested override = %s", got)
	}
	inner()
	if got := Wait(10 * time.Second); got != 300*time.Millisecond {
		t.Errorf("after inner restore = %s", got)
	}
	restore()
	if got := Wait(10 * time.Second); got != 10*time.Second {
		t.Errorf("after restore = %s", got)
	}
}
