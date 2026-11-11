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

package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
)

// msgArtifacts holds the raw --artifact values; msgArtifactRefs the parsed
// references, set by the message command before any send path runs.
var (
	msgArtifacts    []string
	msgArtifactRefs []artifacts.MessageRef
)

// parseArtifactFlags parses --artifact values (scion://artifact/<id>[@<seq>]
// or a bare id) into canonical references. Duplicates of the same artifact
// and more than artifacts.MaxMessageRefs references are usage errors.
func parseArtifactFlags(values []string) ([]artifacts.MessageRef, error) {
	if len(values) > artifacts.MaxMessageRefs {
		return nil, newUsageError("too many --artifact references: %d (max %d)", len(values), artifacts.MaxMessageRefs)
	}
	refs := make([]artifacts.MessageRef, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, v := range values {
		id, seq, err := artifacts.ParseRef(v)
		if err != nil {
			return nil, newUsageError("--artifact %q: %v", v, err)
		}
		if seen[id] {
			return nil, newUsageError("--artifact %q: artifact named more than once", v)
		}
		seen[id] = true
		refs = append(refs, artifacts.MessageRef{ArtifactID: id, Seq: seq})
	}
	return refs, nil
}

// appendArtifactRefsToBody appends each reference URL the body does not
// already contain, one per line, so every reader (web chat, plugins, an
// agent on an older hub) sees the reference in the text.
func appendArtifactRefsToBody(body string, refs []artifacts.MessageRef) string {
	var missing []string
	for _, r := range refs {
		if !strings.Contains(body, r.String()) {
			missing = append(missing, r.String())
		}
	}
	if len(missing) == 0 {
		return body
	}
	if body == "" {
		return strings.Join(missing, "\n")
	}
	return body + "\n\n" + strings.Join(missing, "\n")
}

// artifactRefsMetadata returns the message metadata carrying refs, or nil.
// The hub keeps only the references the sender can read.
func artifactRefsMetadata(refs []artifacts.MessageRef) map[string]string {
	if len(refs) == 0 {
		return nil
	}
	return map[string]string{artifacts.MessageMetadataKey: artifacts.EncodeMessageRefs(refs)}
}

// printArtifactWarning reports artifact references the hub did not attach.
func printArtifactWarning(warning string) {
	if warning != "" {
		fmt.Fprintf(os.Stderr, "Warning: %s\n", warning)
	}
}
