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

package harness

import "strings"

// Harness vendors of the single-vendor built-in harnesses.
const (
	VendorAnthropic = "anthropic"
	VendorGoogle    = "google"
	VendorMeta      = "meta"
	VendorOpenAI    = "openai"
	VendorXAI       = "xai"
)

// harnessVendors maps each built-in harness (the harness name in its
// harnesses/<name>/config.yaml) to its vendor. A single-vendor harness maps
// to the vendor of the models it runs; a multi-vendor harness (one that can
// run models from several vendors) maps to its own harness name, because
// the harness alone does not say whose model runs.
var harnessVendors = map[string]string{
	"antigravity": VendorGoogle,
	"claude":      VendorAnthropic,
	"codex":       VendorOpenAI,
	"gemini-cli":  VendorGoogle,
	"grok-build":  VendorXAI,
	"muse-code":   VendorMeta,

	"copilot":  "copilot",
	"hermes":   "hermes",
	"opencode": "opencode",
}

// Vendor returns the vendor of the harness named harnessName (see
// harnessVendors), matched case-insensitively. It returns "" for a custom
// or unknown harness, including "generic".
func Vendor(harnessName string) string {
	return harnessVendors[strings.ToLower(strings.TrimSpace(harnessName))]
}
