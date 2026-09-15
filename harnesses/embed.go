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

package harnesses

//go:generate go run ./gen

import (
	"embed"
	_ "embed"
)

// FS embeds every subdirectory under harnesses/ using a wildcard so that new
// harness directories are discovered automatically without updating a hard-coded
// list. Non-harness entries (gen/, standalone files) are filtered at load time
// in resources/catalog.go by checking for the presence of config.yaml.
//
//go:embed all:*
var FS embed.FS

//go:embed scion_harness.py
var CanonicalHarnessLib []byte
