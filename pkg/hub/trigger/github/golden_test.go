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

package github

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "rewrite the golden files under testdata")

type goldenSnapshot struct {
	Handled          bool                `json:"handled"`
	SourceKind       string              `json:"source_kind"`
	SourceID         string              `json:"source_id"`
	DeliveryID       string              `json:"delivery_id"`
	Type             string              `json:"type"`
	Subject          string              `json:"subject"`
	OccurredAt       string              `json:"occurred_at"`
	InstallationID   int64               `json:"installation_id"`
	RepoFullName     string              `json:"repo_full_name"`
	SenderLogin      string              `json:"sender_login"`
	SenderID         int64               `json:"sender_id"`
	SenderType       string              `json:"sender_type"`
	PayloadBytes     int                 `json:"payload_bytes"`
	Truncated        bool                `json:"truncated"`
	Attributes       map[string]string   `json:"attributes"`
	Lists            map[string][]string `json:"lists"`
	ProjectedPayload map[string]any      `json:"projected_payload"`
}

// section51Matrix lists every (family, action) pair in Stage 3 design §5.1
// (push excluded; push is G2b).
var section51Matrix = map[string][]string{
	EventPullRequest: {
		"opened",
		"synchronize",
		"reopened",
		"closed",
		"ready_for_review",
		"converted_to_draft",
		"edited",
		"labeled",
		"unlabeled",
		"assigned",
		"unassigned",
		"review_requested",
		"review_request_removed",
	},
	EventIssues: {
		"opened",
		"edited",
		"closed",
		"reopened",
		"labeled",
		"unlabeled",
		"assigned",
		"unassigned",
	},
	EventIssueComment: {
		"created",
		"edited",
		"deleted",
	},
	EventPullRequestReview: {
		"submitted",
		"edited",
		"dismissed",
	},
	EventPullRequestReviewComment: {
		"created",
		"edited",
		"deleted",
	},
}

func renderGoldenSnapshot(t *testing.T, res Result) []byte {
	t.Helper()
	projObj, err := decodeJSONObject(res.Payload)
	require.NoError(t, err)

	snap := goldenSnapshot{
		Handled:          res.Handled,
		SourceKind:       res.SourceKind,
		SourceID:         res.SourceID,
		DeliveryID:       res.DeliveryID,
		Type:             res.Type,
		Subject:          res.Subject,
		OccurredAt:       res.OccurredAt.UTC().Format(time.RFC3339),
		InstallationID:   res.InstallationID,
		RepoFullName:     res.RepoFullName,
		SenderLogin:      res.SenderLogin,
		SenderID:         res.SenderID,
		SenderType:       res.SenderType,
		PayloadBytes:     res.PayloadBytes,
		Truncated:        res.Truncated,
		Attributes:       res.Attributes,
		Lists:            res.Lists,
		ProjectedPayload: projObj,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	require.NoError(t, enc.Encode(snap))
	return buf.Bytes()
}

func TestNormalize_GoldenFixturesAndSection51Coverage(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("testdata", "*.payload.json"))
	require.NoError(t, err)
	require.NotEmpty(t, matches, "expected synthetic webhook fixtures in testdata/")
	sort.Strings(matches)

	fixedReceivedAt := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	opts := Options{
		AppSlug:        "octo-app",
		MentionAliases: []string{"scion"},
		ReceivedAt:     fixedReceivedAt,
	}

	coveredTypes := make(map[string]bool)

	for _, payloadPath := range matches {
		base := strings.TrimSuffix(filepath.Base(payloadPath), ".payload.json")
		parts := strings.SplitN(base, "__", 2)
		require.Len(t, parts, 2, "fixture filename %s must match <family>__<variant>.payload.json", base)
		eventName := parts[0]
		deliveryID := "00000000-0000-4000-8000-" + strings.ReplaceAll(parts[1], "_", "-")
		if len(deliveryID) > 64 {
			deliveryID = deliveryID[:64]
		}

		t.Run(base, func(t *testing.T) {
			raw, err := os.ReadFile(payloadPath)
			require.NoError(t, err)

			res, err := Normalize(eventName, deliveryID, raw, opts)
			require.NoError(t, err)
			require.True(t, res.Handled)

			// Determinism: a second normalisation is byte-identical.
			res2, err := Normalize(eventName, deliveryID, raw, opts)
			require.NoError(t, err)
			assert.Equal(t, string(res.Payload), string(res2.Payload))

			// Universal §16 item 2 invariants across all object-bearing events.
			assert.NotEmpty(t, res.Attributes["author_association"], "author_association must be present on every object-bearing event")
			assert.False(t, res.OccurredAt.IsZero())
			assert.NotEqual(t, fixedReceivedAt, res.OccurredAt, "OccurredAt must come from the object timestamp, not fallback ReceivedAt")
			assert.LessOrEqual(t, res.PayloadBytes, DefaultMaxPayloadBytes)
			_, hasBranch := res.Attributes["branch"]
			assert.False(t, hasBranch, "branch attribute must never be emitted")

			got := renderGoldenSnapshot(t, res)
			got2 := renderGoldenSnapshot(t, res2)
			assert.Equal(t, string(got), string(got2))

			goldenPath := filepath.Join("testdata", base+".golden.json")
			if *update {
				require.NoError(t, os.WriteFile(goldenPath, got, 0o644))
			}
			want, err := os.ReadFile(goldenPath)
			require.NoError(t, err, "missing golden file %s; run with -update", goldenPath)
			assert.Equal(t, string(want), string(got))

			coveredTypes[res.Type] = true
		})
	}

	// Verify every single (family, action) pair in §5.1 has at least one golden test.
	for family, actions := range section51Matrix {
		for _, action := range actions {
			wantType := SourceKind + "." + family + "." + action
			assert.True(t, coveredTypes[wantType], "missing golden fixture coverage for §5.1 event type %s", wantType)
		}
	}
}
