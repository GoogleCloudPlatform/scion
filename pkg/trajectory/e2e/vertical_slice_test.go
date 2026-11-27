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

//go:build !no_sqlite && unix

package e2e

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/trajectory"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/shipper"
)

type e2eEnv struct {
	srv       *hub.Server
	httpSrv   *httptest.Server
	entClient *ent.Client
	store     store.Store
	storage   *storage.LocalStorage
	project   *store.Project
	agent     *store.Agent
	runID     string
	token     string
	homeDir   string
}

func setupE2E(t *testing.T, enableExperiment bool) *e2eEnv {
	t.Helper()
	ctx := context.Background()

	entClient := enttest.NewClient(t)
	st := entadapter.NewCompositeStore(entClient)

	cur := experiments.Default()
	var active []experiments.Experiment
	for _, e := range cur.All() {
		if e.Name == experiments.TokenomicsTrajectoryCapture {
			e.Default = enableExperiment
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, cur.Retired())
	require.NoError(t, err)

	srv, err := hub.New(hub.ServerConfig{
		DevAuthToken:        "e2e-dev-token",
		SharedSigningSecret: "test-e2e-trajectory-signing-secret-32b",
		Experiments:         reg,
	}, st)
	require.NoError(t, err)

	loc, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		Bucket:    "local",
		LocalPath: t.TempDir(),
	})
	require.NoError(t, err)
	srv.SetStorage(loc)

	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)

	proj := &store.Project{
		ID:   api.NewUUID(),
		Name: "e2e-traj-proj",
		Slug: "e2e-traj-proj",
	}
	require.NoError(t, st.CreateProject(ctx, proj))

	runID := "run-" + api.NewUUID()
	agent := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "e2e-traj-agent",
		Name:      "e2e-traj-agent",
		ProjectID: proj.ID,
		RunID:     runID,
		OwnerID:   hub.DevUserID,
		CreatedBy: hub.DevUserID,
		AppliedConfig: &store.AgentAppliedConfig{
			Harness:       "claude",
			HarnessConfig: "claude-default",
		},
		Template: "developer",
	}
	require.NoError(t, st.CreateAgent(ctx, agent))

	_, err = st.EnsureRunAttribution(ctx, &store.RunAttribution{
		RunID:         runID,
		HubID:         "local",
		ProjectID:     proj.ID,
		AgentID:       agent.ID,
		AgentSlug:     agent.Slug,
		Harness:       "claude",
		HarnessConfig: "claude-default",
		Template:      "developer",
	})
	require.NoError(t, err)

	tok, cred, err := srv.GetAgentTokenService().SignAgentToken(hub.AgentTokenGrant{
		AgentID:   agent.ID,
		ProjectID: proj.ID,
		Scopes:    []hub.AgentTokenScope{hub.ScopeAgentStatusUpdate},
	}, runID)
	require.NoError(t, err)
	require.NoError(t, st.CreateAgentCredential(ctx, cred))

	return &e2eEnv{
		srv:       srv,
		httpSrv:   httpSrv,
		entClient: entClient,
		store:     st,
		storage:   loc,
		project:   proj,
		agent:     agent,
		runID:     runID,
		token:     tok,
		homeDir:   t.TempDir(),
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func reassembleGeneration(t *testing.T, ctx context.Context, stor storage.Storage, rows []*ent.TrajectorySegment, gen int) []byte {
	t.Helper()
	var genRows []*ent.TrajectorySegment
	for _, r := range rows {
		if r.Generation == gen && r.Status == telemetrycontract.TrajectorySegmentStatusCommitted {
			genRows = append(genRows, r)
		}
	}
	sort.Slice(genRows, func(i, j int) bool {
		return genRows[i].ByteStart < genRows[j].ByteStart
	})

	var out bytes.Buffer
	for _, r := range genRows {
		// Verify object exists, compressed digest matches, and decompresses cleanly.
		rc, _, err := stor.Download(ctx, r.ObjectPath)
		require.NoError(t, err)
		compBytes, err := io.ReadAll(rc)
		require.NoError(t, rc.Close())
		require.NoError(t, err)
		require.Equal(t, r.CompressedSize, int64(len(compBytes)))
		compSum := sha256.Sum256(compBytes)
		require.Equal(t, r.CompressedSha256, hex.EncodeToString(compSum[:]))

		gz, err := gzip.NewReader(bytes.NewReader(compBytes))
		require.NoError(t, err)
		rawBytes, err := io.ReadAll(gz)
		require.NoError(t, gz.Close())
		require.NoError(t, err)
		rawSum := sha256.Sum256(rawBytes)
		require.Equal(t, r.RawSha256, hex.EncodeToString(rawSum[:]))
		require.Equal(t, r.ByteEnd-r.ByteStart, int64(len(rawBytes)))

		// Verify manifest sidecar exists, matches row & object, and has valid manifest_digest.
		require.NotEmpty(t, r.ManifestPath)
		mrc, _, err := stor.Download(ctx, r.ManifestPath)
		require.NoError(t, err)
		mBytes, err := io.ReadAll(mrc)
		require.NoError(t, mrc.Close())
		require.NoError(t, err)

		var manifest telemetrycontract.TrajectorySegmentManifest
		require.NoError(t, json.Unmarshal(mBytes, &manifest))
		require.Equal(t, telemetrycontract.TrajectoryManifestSchemaVersion, manifest.SchemaVersion)
		require.Equal(t, r.AgentID, manifest.AgentID)
		require.Equal(t, r.RunID, manifest.RunID)
		require.Equal(t, r.ProjectID, manifest.ProjectID)
		require.Equal(t, r.Harness, manifest.Harness)
		require.Equal(t, r.NativeRelativePath, manifest.NativeRelativePath)
		require.Equal(t, r.Generation, manifest.Generation)
		require.Equal(t, r.ByteStart, manifest.ByteStart)
		require.Equal(t, r.ByteEnd, manifest.ByteEnd)
		require.Equal(t, r.RawSha256, manifest.RawSHA256)
		require.Equal(t, r.CompressedSize, manifest.CompressedSize)
		require.Equal(t, r.CompressedSha256, manifest.CompressedSHA256)
		require.Equal(t, r.ObjectPath, manifest.ObjectPath)

		gotDigest := manifest.ManifestDigest
		require.NotEmpty(t, gotDigest)
		manifest.ManifestDigest = ""
		canonical, err := json.Marshal(manifest)
		require.NoError(t, err)
		wantDigest := sha256.Sum256(canonical)
		require.Equal(t, hex.EncodeToString(wantDigest[:]), gotDigest)

		out.Write(rawBytes)
	}
	return out.Bytes()
}

func TestVerticalSlice_BaselineIncrementalReassemblyCrashReplayAndTruncation(t *testing.T) {
	env := setupE2E(t, true)
	ctx := context.Background()

	rel := ".claude/projects/-workspace/sess-vertical.jsonl"
	artPath := filepath.Join(env.homeDir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(artPath), 0o755))

	// 1. Pre-existing bytes before baseline MUST NEVER be uploaded.
	preBaseline := "{\"type\":\"system\",\"text\":\"pre-existing history line 0\"}\n"
	require.NoError(t, os.WriteFile(artPath, []byte(preBaseline), 0o644))

	client := scionhub.NewClientWithConfig(env.httpSrv.URL, env.token, env.agent.ID)
	uploader := scionhub.NewTrajectoryUploader(client)

	s, err := shipper.New(shipper.Config{
		HomeDir:  env.homeDir,
		RunID:    env.runID,
		Harness:  "claude",
		Uploader: uploader,
	})
	require.NoError(t, err)

	baseRes, err := s.Baseline(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, baseRes.ArtifactsTracked)

	// No segments uploaded after baseline alone.
	rows, err := env.entClient.TrajectorySegment.Query().All(ctx)
	require.NoError(t, err)
	require.Empty(t, rows)

	// 2. Incremental capture: append line 1 & 2, run periodic round.
	inc1 := "{\"type\":\"user\",\"text\":\"turn 1 prompt\"}\n{\"type\":\"assistant\",\"text\":\"turn 1 reply\"}\n"
	appendFile(t, artPath, inc1)

	r1, err := s.RunRound(ctx, shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	require.NoError(t, err)
	require.Equal(t, 1, r1.SegmentsShipped)
	require.Equal(t, int64(len(inc1)), r1.BytesShipped)
	require.Equal(t, 0, r1.GenerationBumps)

	// 3a. Crash replay between PUT and commit (simulated kill -9 right after
	// upload succeeded, before commit was called):
	inc2PutOnly := "{\"type\":\"user\",\"text\":\"turn 2a kill between put and commit\"}\n"
	appendFile(t, artPath, inc2PutOnly)

	var prePutCrashSnap []byte
	putOnlyCrashWrap := uploaderFunc(func(uCtx context.Context, seg shipper.Segment) (shipper.ShipResult, error) {
		snap, rerr := os.ReadFile(shipper.StatePath(env.homeDir))
		require.NoError(t, rerr)
		prePutCrashSnap = snap

		prepResp, perr := client.PrepareTrajectorySegment(uCtx, telemetrycontract.TrajectorySegmentPrepareRequest{
			Harness:            seg.Harness,
			NativeRelativePath: seg.NativeRelativePath,
			ArtifactKind:       string(seg.ArtifactKind),
			Generation:         seg.Generation,
			ByteStart:          seg.ByteStart,
			ByteEnd:            seg.ByteEnd,
			RawSHA256:          seg.RawSHA256,
			CompressedSize:     seg.CompressedSize,
			CompressedSHA256:   seg.CompressedSHA256,
			ContentEncoding:    seg.ContentEncoding,
			IncompleteTail:     seg.IncompleteTail,
			SessionID:          seg.SessionID,
			OriginHubID:        seg.OriginHubID,
		})
		require.NoError(t, perr)
		require.NotNil(t, prepResp.Upload)
		_, uerr := client.UploadTrajectorySegment(uCtx, *prepResp.Upload, seg.Compressed)
		require.NoError(t, uerr)
		// Simulate kill -9 BEFORE segments:commit is called!
		return shipper.ShipResult{Outcome: shipper.OutcomeRetryable, Message: "simulated kill -9 between PUT and commit"}, nil
	})
	sPutCrash, err := shipper.New(shipper.Config{
		HomeDir:  env.homeDir,
		RunID:    env.runID,
		Harness:  "claude",
		Uploader: putOnlyCrashWrap,
	})
	require.NoError(t, err)
	_, err = sPutCrash.RunRound(ctx, shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	require.NoError(t, err)
	require.NotEmpty(t, prePutCrashSnap)
	require.NoError(t, os.WriteFile(shipper.StatePath(env.homeDir), prePutCrashSnap, 0o600))

	rPutCatchup, err := s.ResumeCatchup(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, rPutCatchup.SegmentsShipped)
	require.Equal(t, 0, rPutCatchup.GenerationBumps)

	// 3b. Crash replay after commit (lost ACK before watermark advance):
	// Append inc2, snapshot state.json right before commit (with pending persisted)
	// by wrapping the uploader to copy state.json after the real Ship completes
	// (simulating lost ACK / kill -9 right after Hub committed).
	inc2 := "{\"type\":\"user\",\"text\":\"turn 2b crash replay after commit\"}\n"
	appendFile(t, artPath, inc2)

	var preCommitStateSnapshot []byte
	crashWrap := uploaderFunc(func(uCtx context.Context, seg shipper.Segment) (shipper.ShipResult, error) {
		// Snapshot state.json while pending is persisted (before Ship returns).
		snap, rerr := os.ReadFile(shipper.StatePath(env.homeDir))
		require.NoError(t, rerr)
		preCommitStateSnapshot = snap
		// Execute real prepare -> PUT -> commit against real Hub.
		res, serr := uploader.Ship(uCtx, seg)
		require.NoError(t, serr)
		require.Equal(t, shipper.OutcomeCommitted, res.Outcome)
		// Simulate kill -9 before shipper writes the advanced watermark!
		return shipper.ShipResult{Outcome: shipper.OutcomeRetryable, Message: "simulated kill -9"}, nil
	})

	sCrash, err := shipper.New(shipper.Config{
		HomeDir:  env.homeDir,
		RunID:    env.runID,
		Harness:  "claude",
		Uploader: crashWrap,
	})
	require.NoError(t, err)
	_, err = sCrash.RunRound(ctx, shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	require.NoError(t, err)

	// Restore exact pre-commit state.json snapshot (simulating hard kill -9),
	// and also append inc3 so the file grew after the crash!
	require.NotEmpty(t, preCommitStateSnapshot)
	require.NoError(t, os.WriteFile(shipper.StatePath(env.homeDir), preCommitStateSnapshot, 0o600))
	inc3 := "{\"type\":\"assistant\",\"text\":\"turn 2 after crash\"}\n"
	appendFile(t, artPath, inc3)

	// Run ResumeCatchup with the real uploader:
	// - Pending segment inc2 replays idempotently (prepare returns state=committed)
	// - New segment inc3 uploads and commits cleanly
	// - Zero duplicate rows, zero generation bumps!
	rCatchup, err := s.ResumeCatchup(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, rCatchup.SegmentsShipped)
	require.Equal(t, 0, rCatchup.GenerationBumps)

	// 4. Pre-stop Final=true round with trailing partial line (no newline).
	inc4Partial := "{\"type\":\"assistant\",\"text\":\"partial tail before stop\"}"
	appendFile(t, artPath, inc4Partial)
	rFinal, err := s.RunRound(ctx, shipper.RoundOptions{Reason: shipper.ReasonPreStop, Final: true})
	require.NoError(t, err)
	require.Equal(t, 1, rFinal.SegmentsShipped)
	require.Equal(t, 0, rFinal.GenerationBumps)

	// Verify DB rows (5 segments total, all gen 0, no duplicates) and byte-identical reassembly!
	rows, err = env.entClient.TrajectorySegment.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 5)

	wantGen0 := inc1 + inc2PutOnly + inc2 + inc3 + inc4Partial
	gotGen0 := string(reassembleGeneration(t, ctx, env.storage, rows, 0))
	require.Equal(t, wantGen0, gotGen0, "generation 0 reassembly must match post-baseline bytes byte-for-byte")
	require.NotContains(t, gotGen0, "pre-existing history", "pre-baseline bytes must never appear in storage")

	// 5. Truncation: truncate and rewrite the file -> generation bumps to 1,
	// and generation 1 reassembles byte-identically to the rewritten file.
	rewritten := "{\"type\":\"user\",\"text\":\"truncated and rewritten gen 1\"}\n"
	require.NoError(t, os.WriteFile(artPath, []byte(rewritten), 0o644))

	rTrunc, err := s.RunRound(ctx, shipper.RoundOptions{Reason: shipper.ReasonSessionEnd})
	require.NoError(t, err)
	require.Equal(t, 1, rTrunc.GenerationBumps)
	require.Equal(t, 1, rTrunc.SegmentsShipped)

	rows, err = env.entClient.TrajectorySegment.Query().All(ctx)
	require.NoError(t, err)
	require.Len(t, rows, 6)

	gotGen1 := string(reassembleGeneration(t, ctx, env.storage, rows, 1))
	require.Equal(t, rewritten, gotGen1, "generation 1 reassembly must match rewritten file byte-for-byte")
}

func TestVerticalSlice_ExperimentOffBacksOffWithoutStateCorruption(t *testing.T) {
	env := setupE2E(t, false) // tokenomics.trajectory_capture OFF
	ctx := context.Background()

	rel := ".claude/projects/-workspace/sess-off.jsonl"
	artPath := filepath.Join(env.homeDir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(artPath), 0o755))
	require.NoError(t, os.WriteFile(artPath, []byte("{\"baseline\":1}\n"), 0o644))

	var hubCalls atomic.Int32
	countingSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hubCalls.Add(1)
		env.srv.Handler().ServeHTTP(w, r)
	}))
	defer countingSrv.Close()

	client := scionhub.NewClientWithConfig(countingSrv.URL, env.token, env.agent.ID)
	uploader := scionhub.NewTrajectoryUploader(client)

	// Baseline first, then append new bytes so the runner attempts prepare and gets 404.
	s, err := shipper.New(shipper.Config{HomeDir: env.homeDir, RunID: env.runID, Uploader: uploader})
	require.NoError(t, err)
	_, err = s.Baseline(ctx)
	require.NoError(t, err)
	appendFile(t, artPath, "{\"new\":2}\n")

	runner, err := trajectory.NewRunner(trajectory.Config{
		HomeDir:            env.homeDir,
		RunID:              env.runID,
		Uploader:           uploader,
		PeriodicInterval:   20 * time.Millisecond,
		UnavailableBackoff: time.Hour,
		PreStopTimeout:     time.Second,
	})
	require.NoError(t, err)
	require.NoError(t, runner.Start(ctx))

	// Wait for initial resume_catchup 404 + a few ticker intervals to prove backoff stops retries.
	time.Sleep(100 * time.Millisecond)
	runner.PreStop(ctx)

	require.Equal(t, int32(1), hubCalls.Load(), "expected exactly 1 prepare probe (404) followed by backoff")

	// State file must remain valid JSON and watermark must not have advanced.
	st, ok, err := shipper.LoadState(env.homeDir)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(len("{\"baseline\":1}\n")), st.Artifacts[rel].RawCommittedOffset)

	rows, err := env.entClient.TrajectorySegment.Query().All(ctx)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestVerticalSlice_WrongAgentTokenRefused(t *testing.T) {
	env := setupE2E(t, true)
	ctx := context.Background()

	// Create a second agent B and mint a valid token for B.
	agentB := &store.Agent{
		ID:        api.NewUUID(),
		Slug:      "other-agent",
		Name:      "other-agent",
		ProjectID: env.project.ID,
		RunID:     "run-" + api.NewUUID(),
		OwnerID:   hub.DevUserID,
		CreatedBy: hub.DevUserID,
	}
	require.NoError(t, env.store.CreateAgent(ctx, agentB))
	tokB, credB, err := env.srv.GetAgentTokenService().SignAgentToken(hub.AgentTokenGrant{
		AgentID:   agentB.ID,
		ProjectID: env.project.ID,
		Scopes:    []hub.AgentTokenScope{hub.ScopeAgentStatusUpdate},
	}, agentB.RunID)
	require.NoError(t, err)
	require.NoError(t, env.store.CreateAgentCredential(ctx, credB))

	rel := ".claude/projects/-workspace/sess-wrongtok.jsonl"
	artPath := filepath.Join(env.homeDir, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(artPath), 0o755))
	require.NoError(t, os.WriteFile(artPath, nil, 0o644))

	// Point sciontool client at agent A's ID but authenticate with agent B's token.
	wrongClient := scionhub.NewClientWithConfig(env.httpSrv.URL, tokB, env.agent.ID)
	uploader := scionhub.NewTrajectoryUploader(wrongClient)

	s, err := shipper.New(shipper.Config{
		HomeDir:  env.homeDir,
		RunID:    env.runID,
		Uploader: uploader,
	})
	require.NoError(t, err)
	_, err = s.Baseline(ctx)
	require.NoError(t, err)

	appendFile(t, artPath, "{\"type\":\"user\",\"text\":\"should be refused\"}\n")
	res, err := s.RunRound(ctx, shipper.RoundOptions{Reason: shipper.ReasonPeriodic})
	require.NoError(t, err)
	require.Equal(t, 0, res.SegmentsShipped)
	require.Contains(t, res.ArtifactErrors, rel)
	require.Contains(t, res.ArtifactErrors[rel].Error(), "403")

	rows, err := env.entClient.TrajectorySegment.Query().All(ctx)
	require.NoError(t, err)
	require.Empty(t, rows)
}

type uploaderFunc func(ctx context.Context, seg shipper.Segment) (shipper.ShipResult, error)

func (f uploaderFunc) Ship(ctx context.Context, seg shipper.Segment) (shipper.ShipResult, error) {
	return f(ctx, seg)
}
