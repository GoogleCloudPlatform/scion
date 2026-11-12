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
	"context"
	"errors"
	"fmt"
)

// resetAgentHome is not supported on this platform: agent provisioning is
// unix-only. It fails closed and changes nothing.
func resetAgentHome(ctx context.Context, agentDir, agentHome, resetID string) error {
	return fmt.Errorf("reset agent home: %w on this platform", errors.ErrUnsupported)
}
