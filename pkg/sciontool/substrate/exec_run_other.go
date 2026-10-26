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

package substrate

import (
	"context"
	"time"
)

// runExec reports failure: running a command as a workload user needs
// setuid credentials and process groups, which this platform lacks.
func runExec(ctx context.Context, user string, argv []string, stdin []byte, timeout time.Duration) ExecResponse {
	return ExecResponse{ExitCode: -1, Stderr: "exec is not supported on this platform"}
}
