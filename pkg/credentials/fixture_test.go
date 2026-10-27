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

package credentials

import (
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/util/privatefs"
)

// writePrivateFixture writes a credentials file as scion does (mode 0600;
// on Windows an owner-only ACL), so the load accepts it on every OS.
func writePrivateFixture(t *testing.T, path string, data []byte) {
	t.Helper()
	tree, err := privatefs.OpenTree(filepath.Dir(path), privatefs.TreeOptions{CreateAnchor: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tree.Close() }()
	if err := tree.WriteFile(filepath.Base(path), data, privatefs.Private); err != nil {
		t.Fatal(err)
	}
}
