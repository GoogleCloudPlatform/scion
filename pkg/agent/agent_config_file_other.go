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
	"fmt"
)

// errAgentConfigIOUnsupported is returned where the descriptor primitives the
// confined agent-config I/O needs are unavailable (non-unix). A Runtime
// Broker runs on unix only; elsewhere these refuse rather than fall back to
// path-based I/O.
var errAgentConfigIOUnsupported = fmt.Errorf("agent config file I/O: %w on this platform", errors.ErrUnsupported)

// WriteAgentConfigFile refuses on this platform (errAgentConfigIOUnsupported);
// see the unix version.
func WriteAgentConfigFile(agentDir string, data []byte) error {
	return errAgentConfigIOUnsupported
}

// ReadAgentConfigFile refuses on this platform (errAgentConfigIOUnsupported);
// see the unix version.
func ReadAgentConfigFile(agentDir string) ([]byte, error) {
	return nil, errAgentConfigIOUnsupported
}
