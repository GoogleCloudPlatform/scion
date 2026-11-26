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

package messagepolicy

import (
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// PolicyPromptText renders user-visible prompt text from normalized decisions,
// secret requests, and command requests into deterministic labelled lines in
// input order so CEL and System One message policies evaluate prompt content
// alongside the intro body (M3 Slice A, ptone/scion#8049).
//
// Included fields:
//   - Decisions: Question, each Choice.Label and Choice.Description, and
//     Other.Label, Other.Description, and Other.Placeholder.
//   - Secret requests: Key (the user-visible requested variable name) and
//     Description.
//   - Command request: Description (rationale), Workdir (when specified; shown
//     to the approving human in RenderCommandRequestFallback and the CLI
//     confirmation view), and Script.
//
// Never included: submitted secret values or credentials, SecretRequest.Target,
// StandaloneURL or any token-bearing link, Decision.Key, Choice.ID,
// Decision.DefaultChoiceID, CommandRequest.Key, CommandRequest.TimeoutS,
// CommandRequest.ShareOutputRequested, or CommandRequest.CommandSHA256.
func PolicyPromptText(
	decisions []messages.Decision,
	secretRequests []messages.SecretRequest,
	commandRequest *messages.CommandRequest,
) string {
	var lines []string

	for _, d := range decisions {
		if d.Question != "" {
			lines = append(lines, "Decision question: "+d.Question)
		}
		for _, c := range d.Choices {
			if c.Label != "" {
				lines = append(lines, "Choice: "+c.Label)
			}
			if c.Description != "" {
				lines = append(lines, "Choice description: "+c.Description)
			}
		}
		if d.Other != nil {
			if d.Other.Label != "" {
				lines = append(lines, "Other option: "+d.Other.Label)
			}
			if d.Other.Description != "" {
				lines = append(lines, "Other description: "+d.Other.Description)
			}
			if d.Other.Placeholder != "" {
				lines = append(lines, "Other placeholder: "+d.Other.Placeholder)
			}
		}
	}

	for _, sr := range secretRequests {
		if sr.Key != "" {
			lines = append(lines, "Secret request key: "+sr.Key)
		}
		if sr.Description != "" {
			lines = append(lines, "Secret request description: "+sr.Description)
		}
	}

	if commandRequest != nil {
		if commandRequest.Description != "" {
			lines = append(lines, "Command description: "+commandRequest.Description)
		}
		if commandRequest.Workdir != "" {
			lines = append(lines, "Command workdir: "+commandRequest.Workdir)
		}
		if commandRequest.Script != "" {
			lines = append(lines, "Command script:\n"+commandRequest.Script)
		}
	}

	return strings.Join(lines, "\n")
}
