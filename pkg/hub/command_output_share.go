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

	"github.com/GoogleCloudPlatform/scion/pkg/agentcontrol"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// loadSharedCommandRunOutputTail loads, redacts, and bounds (<= 4 KiB UTF-8)
// the captured output for a command run ONLY when the approver opted into
// sharing (run.ShareOutput == true). Raw output is never returned.
//
// This file is allow-listed in TestExecOutputReaders for GetActionRunOutput,
// and callers of loadSharedCommandRunOutputTail are separately allow-listed to
// command_run_exec.go (outcome delivery fallback) and command_requests_handlers.go
// (agent self-service list).
func (s *Server) loadSharedCommandRunOutputTail(ctx context.Context, run *store.CommandRun) (string, int) {
	if run == nil || !run.ShareOutput {
		return "", 0
	}
	if run.OutputStatus != agentcontrol.ExecOutputCaptured && run.OutputStatus != agentcontrol.ExecOutputTruncated {
		return "", 0
	}
	content, err := s.store.GetActionRunOutput(ctx, run.ID)
	if err != nil || len(content) == 0 {
		return "", 0
	}
	return redactAndBoundCommandOutput(content)
}
