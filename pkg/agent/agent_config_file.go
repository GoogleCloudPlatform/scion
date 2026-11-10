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
	"fmt"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// agentConfigFileName is the scion-agent config file the broker reads and
// writes in an agent directory.
const agentConfigFileName = "scion-agent.json"

// maxAgentConfigFileBytes bounds a scion-agent.json read from an agent
// directory (ReadAgentConfigFile).
const maxAgentConfigFileBytes = 4 << 20

// errAgentConfigEntryRefused is wrapped by WriteAgentConfigFile and
// ReadAgentConfigFile when the agent directory's scion-agent.json entry is
// not a plain single-link regular file (a symbolic link, dangling or not, a
// hard link, a directory or a device). It wraps config.ErrAgentStateConflict
// and never names a path.
var errAgentConfigEntryRefused = fmt.Errorf("%w: the agent's scion-agent config file is not a plain regular file; replace it with a regular file, or delete and re-create the agent", config.ErrAgentStateConflict)

// errAgentDirRefused is returned by WriteAgentConfigFile and
// ReadAgentConfigFile when the agent directory itself is a symbolic link (or
// not a directory). It wraps config.ErrAgentStateConflict and never names a
// path.
var errAgentDirRefused = fmt.Errorf("%w: the agent directory is a symbolic link or not a directory; replace it with the directory itself, or delete and re-create the agent", config.ErrAgentStateConflict)
