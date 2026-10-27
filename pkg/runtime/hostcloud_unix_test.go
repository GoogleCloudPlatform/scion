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

//go:build unix

package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// A mode-000 file is unreadable only on unix; elsewhere the permission bits
// do not deny reads.
func TestDetectCloudHost_UnreadableDMIIsNotDetected(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files")
	}
	root := writeFakeRoot(t, dmi(map[string]string{"sys_vendor": "Amazon EC2"}))
	p := filepath.Join(root, "sys/class/dmi/id/sys_vendor")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	if got := detectCloudHost(root, "linux"); got.Detected {
		t.Fatalf("unreadable DMI detected as cloud: %+v", got)
	}
}
