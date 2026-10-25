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

package lifecycleevents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attributionDocTexts are the consumer and operator texts of the WP1
// attribution design v2.8 (sections 2, 3.3, 3.5 and 7.3), pinned verbatim
// in the docs-site reference (design 10.3).
var attributionDocTexts = []string{
	"On a lifecycle event, an absent `launch_id` means the run attribution is unknown or unverified: the producer could not prove which launch the transition belongs to. It does NOT mean zero usage, that no run existed, or a launch-to-generation mapping, and it does not open or close a matched usage interval. Consumers must not join usage to such an event by launch, must not pair launch-less events by adjacency, and must not infer a launch from neighbouring events. On a closure without `launch_id`, `incarnation_id` and `segment_metadata_revision` describe an unattributed segment; they also must not be used to infer the launch. The transition itself is real and counts in coverage accounting. Usage that cannot be attributed stays in totals and in coverage accounting as unmatched; it is never treated as zero and never dropped.",
	"A closing event without `launch_id` can follow an earlier `started` that named a run. The producer then could not prove the pairing at close time, for example after a run-mismatch delete. This does not retract the earlier event, and it is not zero usage. Do not infer the run from the earlier event or from `incarnation_id` and `segment_metadata_revision`. The matched interval is simply not available.",
	"A segment whose stored identity is malformed (only reachable through corruption or an unsupported writer) is discarded without a closing event. The stream marks the loss with one `resynced` event (`cause=unobserved_change`, `gap=true`, no `launch_id`), and the `lifecycle_attribution_stale_total` diagnostic and a log line record it too. That `resynced` signals incomplete transition history. It is not a replacement terminal, a proven run mapping, a matched interval or evidence of zero usage.",
	"Agents that existed before attribution tracking was enabled, whose tracking state was lost, or that went through many delete-and-restore cycles keep an \"unknown history\" mode. For them, a failed start that reverts to the previous run leaves the following events without `launch_id` until the next qualifying fresh mint observed by the hub, which restores normal attribution for the newly minted run. A successful start alone is not that trigger, and it is not evidence that observation was continuous. Do not delete agents to clear this mode.",
	`Run attribution in lifecycle events assumes the lifecycle observer ran continuously, on compatible hub builds, since a known baseline. After the observer was off, after a downgrade, or with mixed hub versions writing agents, attribution for the affected agents is not guaranteed for that period: changes that returned to the same run while unobserved cannot be detected. Treat such periods as unverified.`,
}

// TestAttributionDocsPinned checks that the docs-site lifecycle events
// reference carries every attribution text verbatim.
func TestAttributionDocsPinned(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash("../../docs-site/src/content/docs/reference/server-config.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	for _, text := range attributionDocTexts {
		if !strings.Contains(doc, text) {
			t.Errorf("docs-site reference is missing the attribution text:\n%s", text)
		}
	}
	if !strings.Contains(doc, "lifecycle_attribution_stale") {
		t.Error("docs-site diagnostics do not list lifecycle_attribution_stale")
	}
}
