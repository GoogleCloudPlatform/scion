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
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	agentpkg "github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer/bundle"
)

const (
	// defaultMoveBundlePollInterval is the broker job-status poll interval
	// during bundle export and import (design §3.5).
	defaultMoveBundlePollInterval = 2 * time.Second
	// defaultMoveBundleTouchInterval is how often the worker bumps the
	// reincarnation record's updated_at while polling a long export/import
	// job so the 30-minute stale sweep never reaps a live transfer (design §3.7).
	defaultMoveBundleTouchInterval = 30 * time.Second
	// defaultMoveBundleProgressInterval throttles CAS-gated agent progress
	// message writes while polling so long transfers do not spam row writes.
	defaultMoveBundleProgressInterval = 10 * time.Second
	// moveBundleMaxConsecutivePollErrors is the number of consecutive
	// transient transport/5xx errors on status polls before the move fails.
	// 404, broker job state=failed, and pollCtx expiry remain immediately
	// definitive.
	moveBundleMaxConsecutivePollErrors = 3

	// Fixed error prefixes required by design §3.2.
	moveBundleErrPrefixExport = "bundle export failed:"
	moveBundleErrPrefixImport = "bundle import failed:"
	moveBundleErrPrefixLimit  = "bundle exceeds limit:"
)

func (s *Server) moveBundlePollEvery() time.Duration {
	if s.moveBundlePollInterval > 0 {
		return s.moveBundlePollInterval
	}
	return defaultMoveBundlePollInterval
}

func (s *Server) moveBundleTouchEvery() time.Duration {
	if s.moveBundleTouchInterval > 0 {
		return s.moveBundleTouchInterval
	}
	return defaultMoveBundleTouchInterval
}

func (s *Server) moveBundleProgressEvery() time.Duration {
	if s.moveBundleProgressInterval > 0 {
		return s.moveBundleProgressInterval
	}
	return defaultMoveBundleProgressInterval
}

// touchReincarnation bumps agent_reincarnations.updated_at via a same-state
// CAS (state == expectState) and mirrors that timestamp onto
// agents.reincarnation_updated_at via updateReincarnationStep (design §3.7).
//
// Returns (true, nil) when the CAS succeeded, (false, nil) when another writer
// (such as the stale sweep) already moved the record out of expectState, or
// (false, err) on a persistent store error. When message is non-nil it is
// written in the same agent-row update.
func (s *Server) touchReincarnation(ctx context.Context, agentID, rid, expectState string, message *string) (bool, error) {
	touchNow, ok, err := s.tryAdvanceReincarnation(ctx, rid, expectState, expectState, reincarnationStepMaxAttempts, nil)
	if err != nil || !ok {
		return ok, err
	}
	upd := reincarnationStepUpdate{
		reincarnationState: expectState,
		now:                touchNow,
		message:            message,
	}
	if _, err := s.updateReincarnationStep(ctx, agentID, upd, reincarnationStepMaxAttempts); err != nil {
		s.agentLifecycleLog.Warn("move bundle: failed to bump agent reincarnation_updated_at on touch",
			"agent_id", agentID, "reincarnation_id", rid, "state", expectState, "error", err)
	}
	return true, nil
}

// cleanupMoveBundleStaging removes all staged objects for reincarnationID
// best-effort using storage.DirPrefix(storage.MoveBundlePath(...)) (design
// §3.6). It validates reincarnationID via MoveBundlePath and never deletes
// with an unvalidated or empty ID.
func (s *Server) cleanupMoveBundleStaging(ctx context.Context, reincarnationID string) {
	stor := s.GetStorage()
	if stor == nil {
		return
	}
	basePath, err := storage.MoveBundlePath(s.HubID(), reincarnationID)
	if err != nil {
		s.agentLifecycleLog.Warn("move bundle: refusing to clean up staging with invalid reincarnation ID",
			"reincarnation_id", reincarnationID, "error", err)
		return
	}
	prefix := storage.DirPrefix(basePath)
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := stor.DeletePrefix(cleanCtx, prefix); err != nil {
		s.agentLifecycleLog.Warn("move bundle: best-effort staging cleanup failed",
			"reincarnation_id", reincarnationID, "prefix", prefix, "error", err)
	}
}

// runMoveBundleExport executes steps 4 and 5 of a bundle move (design §3.5):
// starts the export job on the source broker, polls until completion while
// touching the reincarnation record and updating agent.Message, verifies every
// uploaded part against Hub storage and limits, canonicalises the manifest,
// and persists manifest.json to Hub storage.
//
// Returns (manifest, manifestHash, casLost, err). When casLost is true, the
// caller must stop immediately without any side effects.
func (s *Server) runMoveBundleExport(ctx context.Context, md agentMoveDispatcher, agent *store.Agent, reincarnationID string, lastMsg *string) (bundle.Manifest, string, bool, error) {
	limits := s.MoveBundleExportLimits()
	secretTargets, err := s.moveBundleSecretFileTargets(ctx, agent)
	if err != nil {
		return bundle.Manifest{}, "", false, errors.New(formatMoveBundleError(moveBundleErrPrefixExport, err.Error()))
	}

	initialMsg := moveBundleProgressMessage("uploading snapshot", api.MoveBundleProgress{})
	if touched, touchErr := s.touchReincarnation(ctx, agent.ID, reincarnationID, store.AgentReincarnationStateStopping, reincarnateStrPtr(initialMsg)); touchErr == nil && !touched {
		return bundle.Manifest{}, "", true, nil
	} else if touchErr == nil && lastMsg != nil {
		*lastMsg = initialMsg
	}

	status, err := md.DispatchMoveBundleExport(ctx, agent, reincarnationID, limits, secretTargets)
	if err != nil {
		return bundle.Manifest{}, "", false, errors.New(formatMoveBundleError(moveBundleErrPrefixExport, dispatchFailureText(err)))
	}

	finalStatus, casLost, err := s.pollMoveBundleJob(ctx, moveBundlePollParams{
		agentID:         agent.ID,
		reincarnationID: reincarnationID,
		expectState:     store.AgentReincarnationStateStopping,
		actionLabel:     "uploading snapshot",
		errPrefix:       moveBundleErrPrefixExport,
		notFoundReason:  "export job not found on source broker (broker may have restarted)",
		timeout:         limits.Timeout(),
		initial:         status,
		lastMsg:         lastMsg,
		poll: func(pctx context.Context) (*api.MoveBundleJobStatus, error) {
			return md.DispatchMoveBundleExportStatus(pctx, agent, reincarnationID)
		},
	})
	if casLost || err != nil {
		return bundle.Manifest{}, "", casLost, err
	}
	if finalStatus.Manifest == nil {
		return bundle.Manifest{}, "", false, errors.New(formatMoveBundleError(moveBundleErrPrefixExport, "source broker completed export without a manifest"))
	}

	manifest, manifestHash, err := s.verifyAndPersistMoveBundleManifest(ctx, reincarnationID, finalStatus.Manifest, limits)
	if err != nil {
		return bundle.Manifest{}, "", false, err
	}
	return manifest, manifestHash, false, nil
}

// verifyAndPersistMoveBundleManifest executes step 5 (design §3.5, §3.8):
// validates the manifest structure and reincarnation ID, checks totals against
// limits, verifies every part exists in Hub object storage with exact size
// matching Part.Size, canonicalises via bundle.CanonicalManifest, and uploads
// manifest.json to the Hub-derived key.
func (s *Server) verifyAndPersistMoveBundleManifest(ctx context.Context, reincarnationID string, manifest *bundle.Manifest, limits api.MoveBundleLimits) (bundle.Manifest, string, error) {
	stor := s.GetStorage()
	if !isMoveBundleStorageSupported(stor) {
		return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport, moveBundleStorageCheck(stor)))
	}
	if manifest == nil {
		return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport, "manifest is nil"))
	}
	if err := manifest.Validate(); err != nil {
		return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport, "invalid manifest: "+err.Error()))
	}
	if manifest.ReincarnationID != reincarnationID {
		return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport, "manifest reincarnation ID mismatch"))
	}

	for _, rootName := range []string{bundle.RootWorkspace, bundle.RootHome} {
		rm, ok := manifest.Roots[rootName]
		if !ok {
			return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport,
				fmt.Sprintf("manifest missing required root %q", rootName)))
		}
		rootLim := limits.WorkspaceLimits()
		if rootName == bundle.RootHome {
			rootLim = limits.HomeLimits()
		}
		if rm.Bytes > rootLim.MaxBytes {
			return bundle.Manifest{}, "", fmt.Errorf("%s %s bytes %s > %s",
				moveBundleErrPrefixLimit, rootName, formatMoveBundleBytes(rm.Bytes), formatMoveBundleBytes(rootLim.MaxBytes))
		}
		if rm.Entries > rootLim.MaxEntries {
			return bundle.Manifest{}, "", fmt.Errorf("%s %s entries %d > %d",
				moveBundleErrPrefixLimit, rootName, rm.Entries, rootLim.MaxEntries)
		}
		if rm.PartSize > rootLim.PartSize {
			return bundle.Manifest{}, "", fmt.Errorf("%s %s part size %d > %d",
				moveBundleErrPrefixLimit, rootName, rm.PartSize, rootLim.PartSize)
		}
		maxParts := bundle.MaxPartsFor(rootLim.MaxBytes, rm.PartSize)
		if len(rm.Parts) > maxParts || len(rm.Parts) > rootLim.MaxParts() {
			return bundle.Manifest{}, "", fmt.Errorf("%s %s parts %d > %d",
				moveBundleErrPrefixLimit, rootName, len(rm.Parts), maxParts)
		}
		for i, p := range rm.Parts {
			if p.Size > rootLim.PartSize || p.Size > rm.PartSize {
				return bundle.Manifest{}, "", fmt.Errorf("%s %s part %d size %d > %d",
					moveBundleErrPrefixLimit, rootName, p.Index, p.Size, rm.PartSize)
			}
			if i < len(rm.Parts)-1 && p.Size != rm.PartSize {
				return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport,
					fmt.Sprintf("non-final part %s/%d size %d != part size %d", rootName, p.Index, p.Size, rm.PartSize)))
			}
			partKey, err := storage.MoveBundlePartPath(s.HubID(), reincarnationID, rootName, p.Index)
			if err != nil {
				return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport, "invalid part key: "+err.Error()))
			}
			obj, err := stor.GetObject(ctx, partKey)
			if err != nil {
				return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport,
					fmt.Sprintf("verify part %s/%d: %v", rootName, p.Index, err)))
			}
			if obj == nil || obj.Size != p.Size {
				gotSize := int64(-1)
				if obj != nil {
					gotSize = obj.Size
				}
				return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport,
					fmt.Sprintf("part %s/%d size mismatch (stored %d, manifest %d)", rootName, p.Index, gotSize, p.Size)))
			}
		}
	}

	canonBytes, manifestHash, err := bundle.CanonicalManifest(*manifest)
	if err != nil {
		return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport, "canonicalise manifest: "+err.Error()))
	}
	manifestKey, err := storage.MoveBundleManifestPath(s.HubID(), reincarnationID)
	if err != nil {
		return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport, "invalid manifest key: "+err.Error()))
	}
	if _, err := stor.Upload(ctx, manifestKey, bytes.NewReader(canonBytes), storage.UploadOptions{
		ContentType: "application/json",
		Idempotent:  true,
	}); err != nil {
		return bundle.Manifest{}, "", errors.New(formatMoveBundleError(moveBundleErrPrefixExport, "persist manifest: "+err.Error()))
	}
	return *manifest, manifestHash, nil
}

// runMoveBundleImport executes step 8 of a bundle move (design §3.5): starts
// the import job on the target broker with the Hub-persisted manifest and its
// canonical hash, and polls until completion while touching the record every
// 30s and updating agent.Message.
//
// Returns (casLost, err). When casLost is true, the caller must stop
// immediately without any side effects.
func (s *Server) runMoveBundleImport(ctx context.Context, md agentMoveDispatcher, agent *store.Agent, reincarnationID string, manifest bundle.Manifest, manifestHash string, lastMsg *string) (bool, error) {
	limits := s.MoveBundleImportLimits()
	initialMsg := moveBundleProgressMessage("restoring snapshot", api.MoveBundleProgress{})
	if touched, touchErr := s.touchReincarnation(ctx, agent.ID, reincarnationID, store.AgentReincarnationStateProvisioning, reincarnateStrPtr(initialMsg)); touchErr == nil && !touched {
		return true, nil
	} else if touchErr == nil && lastMsg != nil {
		*lastMsg = initialMsg
	}

	status, err := md.DispatchMoveBundleImport(ctx, agent, reincarnationID, manifest, manifestHash, limits)
	if err != nil {
		return false, errors.New(formatMoveBundleError(moveBundleErrPrefixImport, dispatchFailureText(err)))
	}

	_, casLost, err := s.pollMoveBundleJob(ctx, moveBundlePollParams{
		agentID:         agent.ID,
		reincarnationID: reincarnationID,
		expectState:     store.AgentReincarnationStateProvisioning,
		actionLabel:     "restoring snapshot",
		errPrefix:       moveBundleErrPrefixImport,
		notFoundReason:  "import job not found on target broker (broker may have restarted)",
		timeout:         limits.Timeout(),
		initial:         status,
		lastMsg:         lastMsg,
		poll: func(pctx context.Context) (*api.MoveBundleJobStatus, error) {
			return md.DispatchMoveBundleImportStatus(pctx, agent, reincarnationID)
		},
	})
	return casLost, err
}

type moveBundlePollParams struct {
	agentID         string
	reincarnationID string
	expectState     string
	actionLabel     string
	errPrefix       string
	notFoundReason  string
	timeout         time.Duration
	initial         *api.MoveBundleJobStatus
	lastMsg         *string
	poll            func(ctx context.Context) (*api.MoveBundleJobStatus, error)
}

func (s *Server) pollMoveBundleJob(ctx context.Context, p moveBundlePollParams) (*api.MoveBundleJobStatus, bool, error) {
	status := p.initial
	if status == nil {
		return nil, false, errors.New(formatMoveBundleError(p.errPrefix, "empty job status response from broker"))
	}

	pollCtx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	pollInterval := s.moveBundlePollEvery()
	touchInterval := s.moveBundleTouchEvery()
	progressInterval := s.moveBundleProgressEvery()
	lastTouch := time.Now()
	lastProgressWrite := time.Now()
	consecutiveErrors := 0

	for {
		switch status.State {
		case api.MoveBundleJobStateSucceeded:
			return status, false, nil
		case api.MoveBundleJobStateFailed:
			reason := status.Error
			if reason == "" {
				reason = "broker job failed"
			}
			return nil, false, errors.New(formatMoveBundleError(p.errPrefix, reason))
		case api.MoveBundleJobStateRunning:
			// Keep polling below.
		default:
			return nil, false, errors.New(formatMoveBundleError(p.errPrefix, fmt.Sprintf("unexpected broker job state %q", status.State)))
		}

		// Every agent-row progress write is CAS-gated through touchReincarnation
		// so a worker whose record was already failed (e.g. by the stale sweep)
		// can never overwrite agent.ReincarnationState back to in-flight.
		msg := moveBundleProgressMessage(p.actionLabel, status.Progress)
		msgChanged := p.lastMsg == nil || msg != *p.lastMsg
		now := time.Now()
		needTouch := now.Sub(lastTouch) >= touchInterval
		needProgress := msgChanged && now.Sub(lastProgressWrite) >= progressInterval
		if needTouch || needProgress {
			var msgPtr *string
			if msgChanged {
				msgPtr = reincarnateStrPtr(msg)
			}
			touched, touchErr := s.touchReincarnation(ctx, p.agentID, p.reincarnationID, p.expectState, msgPtr)
			if touchErr == nil && !touched {
				// Lost CAS ownership (e.g. stale sweep failed the record):
				// stop polling immediately without side effects.
				return nil, true, nil
			}
			if touchErr != nil {
				s.agentLifecycleLog.Warn("move bundle: failed to touch reincarnation record while polling",
					"agent_id", p.agentID, "reincarnation_id", p.reincarnationID, "state", p.expectState, "error", touchErr)
			} else {
				lastTouch = time.Now()
				if msgPtr != nil {
					lastProgressWrite = lastTouch
					if p.lastMsg != nil {
						*p.lastMsg = msg
					}
				}
			}
		}

		timer := time.NewTimer(pollInterval)
		select {
		case <-pollCtx.Done():
			timer.Stop()
			if errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
				return nil, false, errors.New(formatMoveBundleError(p.errPrefix, fmt.Sprintf("timed out after %s", p.timeout)))
			}
			return nil, false, errors.New(formatMoveBundleError(p.errPrefix, pollCtx.Err().Error()))
		case <-timer.C:
		}

		nextStatus, err := p.poll(pollCtx)
		if err != nil {
			if isBrokerNotFound(err) {
				return nil, false, errors.New(formatMoveBundleError(p.errPrefix, p.notFoundReason))
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(pollCtx.Err(), context.DeadlineExceeded) {
				return nil, false, errors.New(formatMoveBundleError(p.errPrefix, fmt.Sprintf("timed out after %s", p.timeout)))
			}
			if pollCtx.Err() != nil {
				return nil, false, errors.New(formatMoveBundleError(p.errPrefix, pollCtx.Err().Error()))
			}
			consecutiveErrors++
			s.agentLifecycleLog.Warn("move bundle: transient status poll error",
				"agent_id", p.agentID, "reincarnation_id", p.reincarnationID, "state", p.expectState,
				"consecutive_errors", consecutiveErrors, "max_consecutive_errors", moveBundleMaxConsecutivePollErrors,
				"error", agentpkg.RedactURLsInText(err.Error()))
			if consecutiveErrors >= moveBundleMaxConsecutivePollErrors {
				return nil, false, errors.New(formatMoveBundleError(p.errPrefix, dispatchFailureText(err)))
			}
			continue
		}
		if nextStatus == nil {
			return nil, false, errors.New(formatMoveBundleError(p.errPrefix, "empty job status response from broker"))
		}
		consecutiveErrors = 0
		status = nextStatus
	}
}

func moveBundleProgressMessage(action string, p api.MoveBundleProgress) string {
	switch {
	case p.BytesTotal > 0:
		return fmt.Sprintf("migrating: %s (%s/%s)", action, formatMoveBundleBytes(p.BytesDone), formatMoveBundleBytes(p.BytesTotal))
	case p.BytesDone > 0:
		return fmt.Sprintf("migrating: %s (%s)", action, formatMoveBundleBytes(p.BytesDone))
	case p.PartsTotal > 0:
		return fmt.Sprintf("migrating: %s (%d/%d parts)", action, p.PartsDone, p.PartsTotal)
	default:
		return "migrating: " + action
	}
}

// formatMoveBundleError redacts any URLs from raw and ensures the returned
// error text begins with either moveBundleErrPrefixLimit (when raw already
// reports a limit failure) or prefix.
func formatMoveBundleError(prefix, raw string) string {
	clean := strings.TrimSpace(agentpkg.RedactURLsInText(raw))
	if clean == "" {
		return prefix + " unknown error"
	}
	if strings.HasPrefix(clean, moveBundleErrPrefixLimit) {
		return clean
	}
	if strings.HasPrefix(clean, prefix) {
		return clean
	}
	return prefix + " " + clean
}

// moveBundleExportFailureAgentMessage builds the design §3.6 agent.Message for
// a post-stop, pre-assign bundle export failure.
func moveBundleExportFailureAgentMessage(srcName, errMsg string) string {
	if srcName == "" {
		srcName = "source broker"
	}
	return fmt.Sprintf("move failed: %s; agent state is intact on %s; `scion start` to resume", errMsg, srcName)
}

func (s *Server) moveSourceBrokerDisplayName(ctx context.Context, brokerID string) string {
	if b, err := s.store.GetRuntimeBroker(ctx, brokerID); err == nil && b != nil {
		return brokerDisplayName(b)
	}
	return brokerID
}

func isBrokerNotFound(err error) bool {
	var se *brokerStatusError
	return errors.As(err, &se) && se != nil && se.StatusCode == http.StatusNotFound
}
