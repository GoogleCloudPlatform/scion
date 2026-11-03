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
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// allowHydratedTemplate registers dir's parent as a template hydration root,
// as a Runtime Broker registers its template cache, so a test can provision
// from dir as from a hydrated template (an absolute template directory). The
// previous roots are restored when the test ends, so no root leaks into
// other tests (the callers are not parallel tests).
func allowHydratedTemplate(t *testing.T, dir string) {
	t.Helper()
	prev := config.TemplateHydrationRoots()
	config.AddTemplateHydrationRoot(filepath.Dir(dir))
	t.Cleanup(func() { config.SetTemplateHydrationRoots(prev...) })
}
