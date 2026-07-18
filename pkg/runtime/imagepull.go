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
	"context"
	"fmt"
)

var harnessImageMap = map[string]string{
	"claude":      "scion-claude:latest",
	"gemini":      "scion-gemini:latest",
	"codex":       "scion-codex:latest",
	"opencode":    "scion-opencode:latest",
	"antigravity": "scion-antigravity:latest",
}

// HarnessImages returns the fully qualified image names needed for the given harness keys.
func HarnessImages(harnesses []string, registry string) []string {
	var images []string
	for _, h := range harnesses {
		base, ok := harnessImageMap[h]
		if !ok {
			continue
		}
		if registry != "" {
			images = append(images, registry+"/"+base)
		} else {
			images = append(images, base)
		}
	}
	return images
}

// PullResult is the per-image result streamed to the caller.
type PullResult struct {
	Image  string `json:"image"`
	Status string `json:"status"` // "queued" | "exists" | "pulling" | "done" | "error"
	Error  string `json:"error,omitempty"`
	Index  int    `json:"index"` // 1-based position in the pull queue
	Total  int    `json:"total"` // total number of images being pulled
}

// PullImages pulls the images for the given harnesses, streaming PullResult
// events to the provided callback. Images are pulled sequentially.
func PullImages(ctx context.Context, rt Runtime, harnesses []string, registry string, onEvent func(PullResult)) error {
	images := HarnessImages(harnesses, registry)
	if len(images) == 0 {
		return fmt.Errorf("no valid harness images to pull")
	}

	total := len(images)
	for _, img := range images {
		onEvent(PullResult{Image: img, Status: "queued"})
	}

	for i, img := range images {
		if err := ctx.Err(); err != nil {
			return err
		}
		exists, err := rt.ImageExists(ctx, img)
		if err != nil {
			onEvent(PullResult{Image: img, Status: "error", Error: err.Error(), Index: i + 1, Total: total})
			continue
		}
		if exists {
			onEvent(PullResult{Image: img, Status: "exists", Index: i + 1, Total: total})
			continue
		}

		onEvent(PullResult{Image: img, Status: "pulling", Index: i + 1, Total: total})
		if err := rt.PullImage(ctx, img); err != nil {
			onEvent(PullResult{Image: img, Status: "error", Error: err.Error(), Index: i + 1, Total: total})
			continue
		}
		onEvent(PullResult{Image: img, Status: "done", Index: i + 1, Total: total})
	}

	return nil
}
