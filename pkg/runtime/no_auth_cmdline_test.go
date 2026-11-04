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

package runtime

import (
	"io/fs"
	"strings"
	"testing"

	harnessesEmbed "github.com/GoogleCloudPlatform/scion/harnesses"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestHarnessCmdLine_CopilotNoAuthRunsNoCopilot (HC2): from the shipped
// Copilot config.yaml, the no-auth line prints the sign-in message and starts
// a shell; it runs no copilot command (no automatic login).
func TestHarnessCmdLine_CopilotNoAuthRunsNoCopilot(t *testing.T) {
	data, err := fs.ReadFile(harnessesEmbed.FS, "copilot/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := config.ParseHarnessConfigYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	nac := entry.NoAuthConfig
	if nac == nil || nac.Behavior != "drop-to-shell" || nac.Message == "" {
		t.Fatalf("copilot no_auth = %+v, want drop-to-shell with a message", nac)
	}

	// As the agent manager fills RunConfig for a drop-to-shell no-auth start.
	cfg := RunConfig{NoAuth: true, NoAuthMessage: nac.Message, NoAuthCommand: nac.Command}
	got, ok := harnessCmdLine(cfg, harnessCmdLineOptions{})
	want := "printf '%s\\n' " + shellQuote(nac.Message) + "; exec bash"
	if !ok || got != want {
		t.Fatalf("harnessCmdLine = %q, %v; want %q", got, ok, want)
	}
	// The message itself names copilot login as an instruction; nothing
	// outside it may.
	if rest := strings.Replace(got, shellQuote(nac.Message), "", 1); strings.Contains(rest, "copilot") {
		t.Fatalf("no-auth line runs copilot: %q", rest)
	}
}
