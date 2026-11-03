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
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs/provisioner"
	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// The one-shot promotion of workspace-root provisioning markers
// (ptone/scion#5836, lock-drop note §3.2-§3.4). Images from before the
// provisioning state directory kept the sentinel, its git exclude line and
// their lock in the workspace root; current code reads and locks only the
// state directory. At every start the hub, which mounts the share, runs
// provision.PromoteLegacyMarker for every project tree on its shared
// Layout that still has something to promote, under the project's
// provisioning lock, in the background (never blocking start or
// readiness). The step is idempotent: a tree with nothing to promote is
// skipped without a lock or a write. It goes away in a later release.

// legacyProvisionPromotionTimeout bounds one promotion pass.
const legacyProvisionPromotionTimeout = 30 * time.Minute

// legacyProvisionPromotionMax bounds the project trees one pass handles;
// the rest are handled by the next start.
const legacyProvisionPromotionMax = 100000

// legacyPromotionCounts summarizes one promotion pass.
type legacyPromotionCounts struct {
	promoted, cleaned, unchanged, failed, notReached int
}

// startLegacyProvisionPromotion runs promoteLegacyProvisionMarkers in the
// background on a shared workspace backend.
func (s *Server) startLegacyProvisionPromotion(ctx context.Context) {
	if !s.workspaceBackendShared() {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("provisioning state promotion: recovered from panic", "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(ctx, legacyProvisionPromotionTimeout)
		defer cancel()
		s.promoteLegacyProvisionMarkers(ctx)
	}()
}

// promoteLegacyProvisionMarkers runs the promotion over every project tree
// of the hub's shared workspace Layout. Each project the step changed, or
// failed on, gets one audit line ("provisioning state promotion", with
// project_id, outcome and what was done); the pass ends with one summary
// line.
func (s *Server) promoteLegacyProvisionMarkers(ctx context.Context) legacyPromotionCounts {
	var counts legacyPromotionCounts
	logger := s.projectsLogger()
	layout, err := s.workspaceLayout()
	if err != nil || !layout.Backend.Shared() {
		if err != nil {
			logger.Warn("provisioning state promotion: no shared workspace layout", "error", sanitizedStorageError(err))
		}
		return counts
	}
	ids, err := layout.ProjectTreeIDs()
	if err != nil {
		logger.Warn("provisioning state promotion: could not list the project trees", "error", sanitizedStorageError(err))
		counts.failed++
		return counts
	}
	p := projectProvisioner(layout)
	for i, id := range ids {
		if ctx.Err() != nil || i >= legacyProvisionPromotionMax {
			counts.notReached = len(ids) - i
			break
		}
		res, err := promoteProjectProvisionState(ctx, layout, p, id)
		switch {
		case err != nil:
			counts.failed++
			logger.Warn("provisioning state promotion", "project_id", id, "outcome", "failed", "error", sanitizedStorageError(err))
		case res.Changed():
			if res.Promoted {
				counts.promoted++
			} else {
				counts.cleaned++
			}
			logger.Info("provisioning state promotion", "project_id", id, "outcome", "changed",
				"promoted", res.Promoted, "marker_removed", res.MarkerRemoved,
				"exclude_removed", res.ExcludeRemoved, "litter_removed", res.LitterRemoved)
		default:
			counts.unchanged++
		}
	}
	if counts.promoted+counts.cleaned+counts.failed+counts.notReached > 0 {
		logger.Info("provisioning state promotion: pass finished",
			"promoted", counts.promoted, "cleaned", counts.cleaned, "unchanged", counts.unchanged,
			"failed", counts.failed, "not_reached", counts.notReached)
	}
	return counts
}

// promoteProjectProvisionState promotes one project tree: nothing when its
// workspace is absent or has nothing to promote; otherwise the state
// directory is opened through the Provisioner (created with the leaf
// contract if missing) and provision.PromoteLegacyMarker runs.
func promoteProjectProvisionState(ctx context.Context, layout projectfs.Layout, p provisioner.Provisioner, projectID string) (provision.LegacyPromotion, error) {
	r, err := layout.Resolve(projectfs.Project{ID: projectID}, "", projectfs.WorkspaceMount(projectfs.ModeSharedPlain, ""))
	if err != nil {
		return provision.LegacyPromotion{}, err
	}
	base, err := r.ResolvedHostBase()
	if err != nil {
		return provision.LegacyPromotion{}, err
	}
	ws := filepath.Join(base, r.RelPath)
	if _, err := r.VerifyNoSymlinks(); err != nil {
		return provision.LegacyPromotion{}, err
	}
	if _, err := os.Lstat(ws); errors.Is(err, fs.ErrNotExist) {
		return provision.LegacyPromotion{}, nil
	} else if err != nil {
		return provision.LegacyPromotion{}, err
	}
	if !provision.LegacyPromotionNeeded(ws) {
		return provision.LegacyPromotion{}, nil
	}
	if _, err := p.OpenStateDir(projectID); err != nil {
		return provision.LegacyPromotion{}, err
	}
	return provision.PromoteLegacyMarker(ctx, ws)
}
