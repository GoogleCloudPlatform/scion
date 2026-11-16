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
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// writeProvisionHomeFiles writes create-time HomeFiles into agentHome under
// .scion/, confined to agentHome via writeHomeFile. Re-validates files as
// defense-in-depth before touching disk.
func writeProvisionHomeFiles(agentHome string, files []api.HomeFile) error {
	if len(files) == 0 {
		return nil
	}
	if err := api.ValidateHomeFiles(files); err != nil {
		return fmt.Errorf("invalid home files: %w", err)
	}
	for _, f := range files {
		if err := writeHomeFile(agentHome, f.Path, f.Content, os.FileMode(f.Mode)); err != nil {
			return fmt.Errorf("write home file %q: %w", f.Path, err)
		}
	}
	return nil
}
