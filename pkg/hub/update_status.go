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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/maintenance"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/version"
	"github.com/GoogleCloudPlatform/scion/pkg/version/update"
)

// hub_settings rows of the update lifecycle.
const (
	// HubSettingSectionPendingUpdate holds the PendingUpdate of an installed
	// release whose hub restart was requested. It is written just before the
	// restart and reconciled, then cleared, on the next boot.
	HubSettingSectionPendingUpdate = "system.pending_update"
	// HubSettingSectionLastUpdate holds the LastUpdate result of the most
	// recent reconciled update.
	HubSettingSectionLastUpdate = "system.last_update"
	// HubSettingSectionReleaseSelection holds the operator's ReleaseSelection.
	// Only a verified update writes it (boot reconciliation), never a
	// settings PUT.
	HubSettingSectionReleaseSelection = "system.release_selection"
)

// LastUpdate statuses.
const (
	LastUpdateVerified = "verified"
	LastUpdateFailed   = "failed"
	// LastUpdateRolledBack is reported when the boot-failure guard restored
	// the previous release (Phase 3 rollback marker); see updateOutcome.
	LastUpdateRolledBack = "rolled_back"
)

// Update modes reported by update-status.
const (
	UpdateModeGit     = "git"
	UpdateModeRelease = "release"
)

// Channel sources reported by update-status. ChannelSourceSelection means
// an operator-initiated update (an explicit target, channel and pin from the
// API or UI) was verified and recorded as system.release_selection; it then
// takes precedence over the configured release_channel. Auto-updates never
// record a selection, so on a hub without one the configured channel stays
// in effect, and a later config edit applies.
const (
	ChannelSourceSelection = "selection"
	ChannelSourceConfig    = "config"
	ChannelSourceDetected  = "detected"
)

// PendingUpdate is the system.pending_update row.
//
// Channel, Pin and SetBy were added in Phase 2. A row written by an older
// hub lacks them; it is still reconciled, but writes no release selection.
type PendingUpdate struct {
	RunID    string `json:"runId"`
	From     string `json:"from"`
	To       string `json:"to"`
	Snapshot string `json:"snapshot"`
	Strategy string `json:"strategy"`
	At       string `json:"at"`
	// Channel is the installed release's channel.
	Channel string `json:"channel,omitempty"`
	// Pin is maintenance.PinLatest or the installed tag.
	Pin string `json:"pin,omitempty"`
	// SetBy is who started the update run (its started_by).
	SetBy string `json:"setBy,omitempty"`
}

// ReleaseSelection is the system.release_selection row: how the operator
// last chose to run this hub, recorded when that update was verified on
// boot (design D5).
type ReleaseSelection struct {
	Mode    string `json:"mode"`    // UpdateModeRelease (UpdateModeGit from Phase 4)
	Channel string `json:"channel"` // the selected channel
	Pin     string `json:"pin"`     // maintenance.PinLatest or a release tag
	Version string `json:"version"` // the version installed by the selection
	SetBy   string `json:"set_by"`
	SetAt   string `json:"set_at"`
	RunID   string `json:"run_id"`
}

// LastUpdate is the system.last_update row.
type LastUpdate struct {
	RunID  string `json:"runId"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"`
	// At is when the outcome was recorded; for LastUpdateRolledBack, when
	// the boot-failure guard rolled back (the marker's "at").
	At string `json:"at"`
	// Restored is the release the boot-failure guard restored (the
	// marker's "to"); set only for LastUpdateRolledBack.
	Restored string `json:"restored,omitempty"`
}

// effectiveUpdateMode is how this hub is updated: the release selection's
// mode when there is one, else "git" for a source-tier hub (rebuild from the
// maintenance repo) and "release" otherwise.
func effectiveUpdateMode(mc MaintenanceConfig, sel *ReleaseSelection) string {
	if sel != nil && sel.Mode != "" {
		return sel.Mode
	}
	if mc.DeploymentTier == "source" {
		return UpdateModeGit
	}
	return UpdateModeRelease
}

// effectiveReleaseChannel is the channel updates are checked and installed
// from: the release selection's channel, else the configured
// release_channel, else the running version's channel ("" for a git build).
// source is ChannelSourceSelection, ChannelSourceConfig or
// ChannelSourceDetected.
func effectiveReleaseChannel(mc MaintenanceConfig, running string, sel *ReleaseSelection) (channel, source string) {
	if sel != nil && sel.Channel != "" {
		return sel.Channel, ChannelSourceSelection
	}
	if mc.ReleaseChannel != "" {
		return mc.ReleaseChannel, ChannelSourceConfig
	}
	return update.DetectChannel(running), ChannelSourceDetected
}

// effectivePin is the release selection's pin, maintenance.PinLatest when
// there is no selection.
func effectivePin(sel *ReleaseSelection) string {
	if sel != nil && sel.Pin != "" {
		return sel.Pin
	}
	return maintenance.PinLatest
}

// readReleaseSelection returns system.release_selection, nil when there is
// none. An unreadable row is ignored with a warning (the hub falls back to
// its configuration, as without a selection); a store error is returned.
func (s *Server) readReleaseSelection(ctx context.Context) (*ReleaseSelection, error) {
	var sel ReleaseSelection
	found, err := s.readHubSettingJSON(ctx, HubSettingSectionReleaseSelection, &sel)
	if found && err != nil {
		s.maintenanceLog.Warn("Ignoring an unreadable release selection", "error", err)
		return nil, nil
	}
	if err != nil || !found {
		return nil, err
	}
	return &sel, nil
}

// selectionFromPending is the release selection a verified pending update
// records. Only an operator-initiated update records one: skip says why
// none is recorded for a row written before Phase 2 (no channel) or for an
// auto-update run (started by system:auto-update). Neither carried an
// operator choice, and recording one would silently override the configured
// release_channel from then on.
func selectionFromPending(p PendingUpdate, at time.Time) (sel ReleaseSelection, skip string) {
	switch {
	case p.Channel == "":
		return ReleaseSelection{}, "the pending update has no channel (written before release selections)"
	case p.SetBy == autoUpdateStartedBy:
		return ReleaseSelection{}, "the update was started by the auto-update policy, not an operator"
	}
	pin := p.Pin
	if pin == "" {
		pin = maintenance.PinLatest
	}
	return ReleaseSelection{
		Mode:    UpdateModeRelease,
		Channel: p.Channel,
		Pin:     pin,
		Version: p.To,
		SetBy:   p.SetBy,
		SetAt:   at.UTC().Format(time.RFC3339),
		RunID:   p.RunID,
	}, ""
}

// updateInstaller returns the Installer detected at startup, or an
// unsupported one when none was provided (tests, or a server built without
// maintenance wiring).
func (s *Server) updateInstaller() maintenance.Installer {
	if inst := s.config.MaintenanceConfig.Installer; inst != nil {
		return inst
	}
	return maintenance.NewUnsupportedInstaller("no update installer was detected at startup", "")
}

// DetectMaintenanceInstaller detects the update Installer for this host from
// the maintenance config. Call it once at startup and store the result in
// MaintenanceConfig.Installer.
func DetectMaintenanceInstaller(ctx context.Context, mc MaintenanceConfig) maintenance.Installer {
	return maintenance.Detect(ctx, maintenance.DetectOptions{
		InstallRoot: mc.InstallRoot,
		RepoPath:    mc.RepoPath,
		BinaryDest:  mc.BinaryDest,
		ServiceName: mc.ServiceName,
	})
}

// readHubSettingJSON decodes hub setting section into v. found is false when
// the row does not exist.
func (s *Server) readHubSettingJSON(ctx context.Context, section string, v any) (found bool, err error) {
	setting, err := s.store.GetHubSetting(ctx, section)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if err := json.Unmarshal(setting.Value, v); err != nil {
		return true, fmt.Errorf("decode %s: %w", section, err)
	}
	return true, nil
}

// writeHubSettingJSON upserts v as hub setting section.
func (s *Server) writeHubSettingJSON(ctx context.Context, section string, v any) error {
	value, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode %s: %w", section, err)
	}
	if _, err := s.store.UpsertHubSetting(ctx, section, json.RawMessage(value), "system", -1, "managed"); err != nil {
		return fmt.Errorf("write %s: %w", section, err)
	}
	return nil
}

// clearPendingUpdateFor deletes system.pending_update when it names runID.
func (s *Server) clearPendingUpdateFor(ctx context.Context, runID string) {
	var p PendingUpdate
	found, err := s.readHubSettingJSON(ctx, HubSettingSectionPendingUpdate, &p)
	if err != nil {
		s.maintenanceLog.Warn("Failed to read pending_update", "error", err)
	}
	if !found || (err == nil && p.RunID != runID) {
		return
	}
	if err := s.store.DeleteHubSetting(ctx, HubSettingSectionPendingUpdate); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.maintenanceLog.Warn("Failed to clear pending_update", "run_id", runID, "error", err)
	}
}

// updateOutcome decides the LastUpdate status of a pending update from the
// version the hub booted on and the boot-failure guard's rollback marker
// not yet seen (nil for none): LastUpdateVerified when the hub runs the
// target, LastUpdateRolledBack when the marker records a rollback from
// the target (markerMatchesPending), LastUpdateFailed otherwise.
func updateOutcome(p PendingUpdate, running string, marker *RollbackMarker) string {
	if p.To != "" && running == p.To {
		return LastUpdateVerified
	}
	if markerMatchesPending(marker, p) {
		return LastUpdateRolledBack
	}
	return LastUpdateFailed
}

// reconcilePendingUpdate runs at startup, before AbortRunningMaintenanceOps.
// When system.pending_update exists it completes the update run it names:
// "completed" with system.last_update status "verified" (the release
// selection recorded from the row, and the update-available notification
// cleared) when the hub now runs the target version, "failed" otherwise
// (the notification and the previous selection stay), with status
// "rolled_back" when the boot-failure guard's rollback marker records the
// rollback from the target. A new rollback marker is recorded as seen, so
// it is reported once. It then clears the row. It
// is idempotent (a re-run after a partial failure converges), tolerates a
// missing run row, and fails any other update-binary run still left
// "restarting" (its restart was never confirmed), so no stale run blocks
// new updates.
func (s *Server) reconcilePendingUpdate(ctx context.Context, running string) error {
	log := s.maintenanceLog

	var p PendingUpdate
	found, err := s.readHubSettingJSON(ctx, HubSettingSectionPendingUpdate, &p)
	if err != nil && !found {
		return fmt.Errorf("read pending_update: %w", err)
	}
	if found && err != nil {
		// An unreadable row cannot be reconciled; drop it so it does not
		// linger, and let the stale-run sweep below fail its run.
		log.Warn("Discarding unreadable pending_update", "error", err)
		p = PendingUpdate{}
	}

	marker := s.newRollbackMarker(ctx)
	if found && p.RunID != "" {
		status := updateOutcome(p, running, marker)
		if err := s.finishPendingRun(ctx, p, status, running); err != nil {
			return err
		}
		last := LastUpdate{
			RunID:  p.RunID,
			From:   p.From,
			To:     p.To,
			Status: status,
			At:     time.Now().UTC().Format(time.RFC3339),
		}
		if status == LastUpdateRolledBack {
			last.At, last.Restored = marker.At, marker.To
		}
		if err := s.writeHubSettingJSON(ctx, HubSettingSectionLastUpdate, last); err != nil {
			return err
		}
		if status == LastUpdateRolledBack {
			// After last_update: a failed write keeps pending_update, and
			// the next boot reports the same marker again.
			if err := s.recordRollbackMarker(ctx, marker, p.RunID); err != nil {
				return err
			}
			marker = nil
		}
		// The operator's choice (channel, pin) takes effect only once the
		// hub runs it. A failed write keeps pending_update, so the next
		// boot records it.
		if status == LastUpdateVerified {
			if sel, skip := selectionFromPending(p, time.Now()); skip == "" {
				if err := s.writeHubSettingJSON(ctx, HubSettingSectionReleaseSelection, sel); err != nil {
					return err
				}
			} else {
				log.Debug("No release selection recorded for the verified update",
					"run_id", p.RunID, "to", p.To, "reason", skip)
			}
		}
		// The update is confirmed only now; the update-binary hook leaves
		// the banner in place so a failed or rolled-back attempt can be
		// retried from it. A newer release, if any, is announced again by
		// the next scheduled check.
		if status == LastUpdateVerified {
			if err := s.store.DeleteHubSetting(ctx, HubSettingSectionUpdateAvailable); err != nil && !errors.Is(err, store.ErrNotFound) {
				log.Warn("Failed to clear update_available after a verified update", "error", err)
			}
		}
		log.Info("Reconciled pending update", "run_id", p.RunID, "from", p.From, "to", p.To,
			"running", running, "status", status)
	}

	if found {
		if err := s.store.DeleteHubSetting(ctx, HubSettingSectionPendingUpdate); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("clear pending_update: %w", err)
		}
	}
	if marker != nil {
		// A rollback with no matching pending update (an update not made
		// by this hub, or a marker older than the update): reported in the
		// log only, once.
		log.Warn("The boot-failure guard rolled back a release that failed to start",
			"from", marker.From, "to", marker.To, "at", marker.At)
		if err := s.recordRollbackMarker(ctx, marker, ""); err != nil {
			log.Warn("Failed to record the rollback marker as seen", "error", err)
		}
	}
	return s.failStaleRestartingRuns(ctx)
}

// finishPendingRun marks the run of a pending update completed or failed.
// A missing run, or one already finished by an earlier pass, is left as is.
func (s *Server) finishPendingRun(ctx context.Context, p PendingUpdate, status, running string) error {
	run, err := s.store.GetMaintenanceRun(ctx, p.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.maintenanceLog.Warn("Pending update names a run that does not exist", "run_id", p.RunID)
			return nil
		}
		return fmt.Errorf("read run %s: %w", p.RunID, err)
	}
	if run.Status != store.MaintenanceStatusRestarting && run.Status != store.MaintenanceStatusRunning {
		return nil
	}

	result := map[string]any{}
	if run.Result != "" {
		_ = json.Unmarshal([]byte(run.Result), &result)
	}
	result["from"], result["to"], result["running"] = p.From, p.To, running
	result["verification"] = status
	now := time.Now()
	var line string
	if status == LastUpdateVerified {
		run.Status = store.MaintenanceStatusCompleted
		line = fmt.Sprintf("\n==> Hub restarted on %s at %s: update verified.\n", running, now.UTC().Format(time.RFC3339))
	} else if status == LastUpdateRolledBack {
		run.Status = store.MaintenanceStatusFailed
		result["error"] = fmt.Sprintf("%q failed to start; the boot-failure guard rolled back and the hub restarted on %q",
			p.To, displayRunningVersion(running))
		line = fmt.Sprintf("\n==> %s failed to start; the boot-failure guard rolled back. Hub restarted on %s at %s.\n",
			p.To, displayRunningVersion(running), now.UTC().Format(time.RFC3339))
	} else {
		run.Status = store.MaintenanceStatusFailed
		result["error"] = fmt.Sprintf("the hub restarted on %q, not the installed %q", displayRunningVersion(running), p.To)
		line = fmt.Sprintf("\n==> Hub restarted on %s at %s, expected %s: update failed.\n",
			displayRunningVersion(running), now.UTC().Format(time.RFC3339), p.To)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode run result: %w", err)
	}
	run.Result = string(resultJSON)
	run.Log += line
	run.CompletedAt = &now
	if err := s.store.UpdateMaintenanceRun(ctx, run); err != nil {
		return fmt.Errorf("update run %s: %w", p.RunID, err)
	}
	return nil
}

// failStaleRestartingRuns fails update-binary runs still "restarting" once
// the pending update is reconciled: their restart can no longer be
// confirmed. Only update-binary restarts the hub, and only its latest runs
// can be in flight.
func (s *Server) failStaleRestartingRuns(ctx context.Context) error {
	runs, err := s.store.ListMaintenanceRuns(ctx, "update-binary", 20)
	if err != nil {
		return fmt.Errorf("list update-binary runs: %w", err)
	}
	for i := range runs {
		run := &runs[i]
		if run.Status != store.MaintenanceStatusRestarting {
			continue
		}
		now := time.Now()
		run.Status = store.MaintenanceStatusFailed
		run.CompletedAt = &now
		resultJSON, _ := json.Marshal(map[string]string{
			"error": "the hub restarted without a pending update record for this run; the update could not be verified",
		})
		run.Result = string(resultJSON)
		if err := s.store.UpdateMaintenanceRun(ctx, run); err != nil {
			return fmt.Errorf("fail stale restarting run %s: %w", run.ID, err)
		}
		s.maintenanceLog.Warn("Failed a restarting update run with no pending update", "run_id", run.ID)
	}
	return nil
}

func displayRunningVersion(v string) string {
	if v == "" {
		return "a git build"
	}
	return v
}

// updateStatusResponse is GET /api/v1/admin/maintenance/update-status.
type updateStatusResponse struct {
	Current       updateStatusCurrent         `json:"current"`
	Mode          updateStatusMode            `json:"mode"`
	Channel       updateStatusChannel         `json:"channel"`
	Pin           string                      `json:"pin"`
	Installer     maintenance.InstallerStatus `json:"installer"`
	UpdatePolicy  string                      `json:"updatePolicy"`
	Downgrade     updateStatusDowngrade       `json:"downgrade"`
	PendingUpdate *PendingUpdate              `json:"pendingUpdate"`
	LastUpdate    *LastUpdate                 `json:"lastUpdate"`
}

type updateStatusCurrent struct {
	Version    string `json:"version"`
	Channel    string `json:"channel"`
	Commit     string `json:"commit"`
	BuildTime  string `json:"buildTime"`
	IsGitBuild bool   `json:"isGitBuild"`
}

type updateStatusMode struct {
	Effective      string `json:"effective"`
	RepoConfigured bool   `json:"repoConfigured"`
	RepoBranch     string `json:"repoBranch"`
}

type updateStatusChannel struct {
	Effective  string `json:"effective"`
	Configured string `json:"configured"`
	// Source is where Effective came from: ChannelSourceSelection (an
	// operator-initiated update's recorded choice; auto-updates never
	// record one), ChannelSourceConfig or ChannelSourceDetected.
	Source string `json:"source"`
}

// runningBuild identifies the running binary for update-status.
type runningBuild struct {
	Version   string
	Commit    string
	BuildTime string
}

func currentRunningBuild() runningBuild {
	return runningBuild{Version: version.Version, Commit: version.GetCommit(), BuildTime: version.BuildTime}
}

// buildUpdateStatus assembles update-status for the running build cur. The
// maintenance repo path is reported only as configured or not.
func (s *Server) buildUpdateStatus(ctx context.Context, cur runningBuild) (*updateStatusResponse, error) {
	mc := s.config.MaintenanceConfig
	sel, err := s.readReleaseSelection(ctx)
	if err != nil {
		return nil, err
	}
	channel, source := effectiveReleaseChannel(mc, cur.Version, sel)
	isGit := !update.IsReleaseTag(cur.Version)
	displayVersion := cur.Version
	if displayVersion == "" {
		// A git build has no version; show its short commit, as
		// version.Short does.
		displayVersion = cur.Commit
		if len(displayVersion) > 8 {
			displayVersion = displayVersion[:8]
		}
	}

	resp := &updateStatusResponse{
		Current: updateStatusCurrent{
			Version:    displayVersion,
			Channel:    update.DetectChannel(cur.Version),
			Commit:     cur.Commit,
			BuildTime:  cur.BuildTime,
			IsGitBuild: isGit,
		},
		Mode: updateStatusMode{
			Effective:      effectiveUpdateMode(mc, sel),
			RepoConfigured: mc.RepoPath != "",
			RepoBranch:     mc.RepoBranch,
		},
		Channel: updateStatusChannel{
			Effective:  channel,
			Configured: mc.ReleaseChannel,
			Source:     source,
		},
		Pin:          effectivePin(sel),
		Installer:    redactInstallerStatus(s.updateInstaller().Status(), mc.RepoPath),
		UpdatePolicy: mc.UpdatePolicy,
		Downgrade: updateStatusDowngrade{
			Floor:              version.DowngradeFloor,
			SnapshotsSupported: s.updateSnapshotter(ctx).SnapshotStatus().Supported,
		},
	}

	var pending PendingUpdate
	if found, err := s.readHubSettingJSON(ctx, HubSettingSectionPendingUpdate, &pending); err != nil {
		return nil, err
	} else if found {
		resp.PendingUpdate = &pending
	}
	var last LastUpdate
	if found, err := s.readHubSettingJSON(ctx, HubSettingSectionLastUpdate, &last); err != nil {
		return nil, err
	} else if found {
		resp.LastUpdate = &last
	}
	return resp, nil
}

// repoPathPlaceholder replaces the maintenance repo path in text that
// update-status returns.
const repoPathPlaceholder = "<maintenance repo>"

// redactInstallerStatus removes the maintenance repo path from the
// installer's reason and hint: update-status reports only whether a repo is
// configured, never its path (installer detection builds its reasons from
// host paths and sudo argv, which include it).
//
// Every spelling is redacted: the configured value (installers format it
// verbatim), its cleaned form (filepath.Join output) and its
// symlink-resolved form (the resolved running binary can lie inside the
// repo). Only whole path segments match, so /srv/repo does not touch
// /srv/repository. A relative repo path is not redacted: it names no
// location and would match unrelated words.
func redactInstallerStatus(st maintenance.InstallerStatus, repoPath string) maintenance.InstallerStatus {
	for _, re := range repoPathPatterns(repoPath) {
		st.Reason = re.ReplaceAllString(st.Reason, repoPathPlaceholder+"${1}")
		st.MigrateHint = re.ReplaceAllString(st.MigrateHint, repoPathPlaceholder+"${1}")
	}
	return st
}

// repoPathPatterns returns, longest spelling first, a pattern per spelling
// of repoPath that matches it as whole path segments.
func repoPathPatterns(repoPath string) []*regexp.Regexp {
	if !filepath.IsAbs(repoPath) {
		return nil
	}
	clean := filepath.Clean(repoPath)
	spellings := []string{strings.TrimRight(repoPath, "/"), clean}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		spellings = append(spellings, resolved)
	}
	seen := map[string]bool{}
	var uniq []string
	for _, p := range spellings {
		if p == "" || p == "/" || seen[p] {
			continue
		}
		seen[p] = true
		uniq = append(uniq, p)
	}
	sort.Slice(uniq, func(i, j int) bool { return len(uniq[i]) > len(uniq[j]) })
	res := make([]*regexp.Regexp, 0, len(uniq))
	for _, p := range uniq {
		res = append(res, regexp.MustCompile(regexp.QuoteMeta(p)+`($|[^A-Za-z0-9._-])`))
	}
	return res
}

// handleUpdateStatus handles GET /api/v1/admin/maintenance/update-status.
// Authorization: enforced by routeGuard via hub.maintenance.execute permission.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	resp, err := s.buildUpdateStatus(r.Context(), currentRunningBuild())
	if err != nil {
		s.maintenanceLog.Error("Failed to build update status", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to read update status", nil)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
