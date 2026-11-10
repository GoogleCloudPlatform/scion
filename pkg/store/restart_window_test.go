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

package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestRestartPreMint(t *testing.T) {
	claimAt := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	before := claimAt.Add(-time.Hour)
	after := claimAt.Add(time.Second)
	for _, tc := range []struct {
		name      string
		held      bool
		kind      StartClaimKind
		runID     string
		runIDAt   *time.Time
		claimAt   *time.Time
		wantMatch bool
	}{
		{"restart, run recorded before the claim", true, StartClaimRestart, "run-1", &before, &claimAt, true},
		{"restart, run time unknown", true, StartClaimRestart, "run-1", nil, &claimAt, true},
		{"restart, new run recorded after the claim", true, StartClaimRestart, "run-2", &after, &claimAt, false},
		{"restart, run recorded at the claim time", true, StartClaimRestart, "run-2", &claimAt, &claimAt, false},
		{"restart, row has no run", true, StartClaimRestart, "", nil, &claimAt, false},
		{"no claim held", false, StartClaimRestart, "run-1", &before, &claimAt, false},
		{"user claim", true, StartClaimUser, "run-1", &before, &claimAt, false},
		{"reincarnate claim", true, StartClaimReincarnate, "run-1", &before, &claimAt, false},
		{"restart, claim time unknown, run time known", true, StartClaimRestart, "run-1", &before, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantMatch, RestartPreMint(tc.held, tc.kind, tc.runID, tc.runIDAt, tc.claimAt))
		})
	}
}

func TestRestartPreMintReport(t *testing.T) {
	for _, tc := range []struct {
		name string
		su   AgentStatusUpdate
		want bool
	}{
		{"heartbeat of the current run", AgentStatusUpdate{ReportSource: StatusReportSourceRuntime, ReportRunID: "run-1"}, true},
		{"run-less heartbeat", AgentStatusUpdate{ReportSource: StatusReportSourceRuntime}, true},
		{"heartbeat of another run", AgentStatusUpdate{ReportSource: StatusReportSourceRuntime, ReportRunID: "run-0"}, false},
		{"agent token of the current run", AgentStatusUpdate{ReportSource: StatusReportSourceAgent, ReportAgentToken: true, ReportRunID: "run-1"}, true},
		{"run-less agent token", AgentStatusUpdate{ReportSource: StatusReportSourceAgent, ReportAgentToken: true}, true},
		{"user or service update", AgentStatusUpdate{ReportSource: StatusReportSourceAgent}, false},
		{"hub-internal write", AgentStatusUpdate{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, RestartPreMintReport(tc.su, "run-1"))
		})
	}
}

func TestRunlessAgentTokenReport(t *testing.T) {
	minted := time.Now()
	runless := AgentStatusUpdate{ReportSource: StatusReportSourceAgent, ReportAgentToken: true}
	assert.True(t, RunlessAgentTokenReport(runless, &minted))
	assert.False(t, RunlessAgentTokenReport(runless, nil), "a row with no minted run is unaffected")
	withRun := runless
	withRun.ReportRunID = "run-1"
	assert.False(t, RunlessAgentTokenReport(withRun, &minted), "a token with a run is the run check's business")
	user := AgentStatusUpdate{ReportSource: StatusReportSourceAgent}
	assert.False(t, RunlessAgentTokenReport(user, &minted), "users and services are not agent tokens")
	heartbeat := AgentStatusUpdate{ReportSource: StatusReportSourceRuntime}
	assert.False(t, RunlessAgentTokenReport(heartbeat, &minted), "run-less heartbeats are not covered")
}

func TestDropReportedStatus(t *testing.T) {
	code := 1
	su := AgentStatusUpdate{Phase: "stopped", Activity: "idle", ExitCode: &code, ExitReason: "oom_killed",
		Message: "m", ContainerStatus: "exited (1)", Heartbeat: true}
	DropReportedStatus(&su)
	assert.Empty(t, su.Phase)
	assert.Empty(t, su.Activity)
	assert.Nil(t, su.ExitCode)
	assert.Empty(t, su.ExitReason)
	assert.Empty(t, su.Message)
	assert.Equal(t, "exited (1)", su.ContainerStatus)
	assert.True(t, su.Heartbeat)
}

func TestRunIDStamp(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Minute)
	within := now.Add(2 * time.Second)
	beyond := now.Add(RestartClaimSkewCap + time.Second)
	atCap := now.Add(RestartClaimSkewCap)
	justBeyond := atCap.Add(time.Nanosecond)
	for _, tc := range []struct {
		name       string
		held       bool
		kind       StartClaimKind
		claimAt    *time.Time
		wantStamp  time.Time
		wantBeyond bool
	}{
		{"no claim", false, StartClaimRestart, &within, now, false},
		{"user claim", true, StartClaimUser, &within, now, false},
		{"restart claim with no time", true, StartClaimRestart, nil, now, false},
		{"restart claim in the past", true, StartClaimRestart, &past, now, false},
		{"restart claim ahead within the cap", true, StartClaimRestart, &within, within, false},
		{"restart claim ahead beyond the cap", true, StartClaimRestart, &beyond, now, true},
		{"restart claim ahead exactly the cap", true, StartClaimRestart, &atCap, atCap, false},
		{"restart claim ahead the cap plus 1ns", true, StartClaimRestart, &justBeyond, now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stamp, over := RunIDStamp(now, tc.held, tc.kind, tc.claimAt)
			assert.Equal(t, tc.wantStamp, stamp)
			assert.Equal(t, tc.wantBeyond, over > 0)
		})
	}
}
