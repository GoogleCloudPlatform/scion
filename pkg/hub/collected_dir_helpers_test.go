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
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
)

// collectTestDir collects dir the way the ResourceStore does (the default
// exclude patterns plus extraExcludes) and opens its root, closed when the
// test ends.
func collectTestDir(t testing.TB, dir string, extraExcludes ...string) *collectedDir {
	t.Helper()
	files, err := transfer.CollectFiles(dir, extraExcludes)
	if err != nil {
		t.Fatalf("collect %s: %v", dir, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("open root %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return newCollectedDir(dir, root, files)
}

// deriveTemplateIndexForTest collects dir and derives its template index,
// failing the test on an error.
func deriveTemplateIndexForTest(t testing.TB, dir, templateName string) templateIndex {
	t.Helper()
	idx, err := deriveTemplateIndexFromCollected(collectTestDir(t, dir), templateName)
	if err != nil {
		t.Fatalf("derive the template index of %s: %v", dir, err)
	}
	return idx
}
