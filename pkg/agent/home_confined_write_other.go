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

package agent

import (
	"errors"
	"os"
)

// The confined agent-home writes (home_confined_write_unix.go) rely on the
// unix descriptor primitives of pkg/sciontool/dirfd. A Runtime Broker runs
// on unix only; elsewhere these writes refuse rather than write without
// that confinement.

var errConfinedWriteUnsupported = errors.New("writing into the agent home is not supported on this platform")

func writeHomeFile(agentHome, rel string, data []byte, mode os.FileMode) error {
	return errConfinedWriteUnsupported
}

func removeHomeFile(agentHome, rel string) error {
	return errConfinedWriteUnsupported
}

func installHomeDir(srcDir, agentHome, dstRel string) error {
	return errConfinedWriteUnsupported
}
