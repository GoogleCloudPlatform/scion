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

package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

type stubTrajectoryPolicyFetcher struct {
	configured bool
	status     *telemetrycontract.TrajectoryCapturePolicyStatus
	err        error
	delay      time.Duration
	calls      int
}

func (s *stubTrajectoryPolicyFetcher) IsConfigured() bool { return s.configured }

func (s *stubTrajectoryPolicyFetcher) GetTrajectoryCapturePolicy(ctx context.Context) (*telemetrycontract.TrajectoryCapturePolicyStatus, error) {
	s.calls++
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.status, s.err
}

func TestFetchTrajectoryCaptureNotice(t *testing.T) {
	t.Run("returns notice for full enforced", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{
			configured: true,
			status: &telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyFull,
				EnforcedByHub: true,
			},
		}
		got := fetchTrajectoryCaptureNotice(context.Background(), f, time.Second)
		assert.Equal(t, telemetrycontract.TrajectoryCaptureNotice(*f.status), got)
	})

	t.Run("returns notice for metadata_only", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{
			configured: true,
			status: &telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy:        telemetrycontract.TrajectoryCapturePolicyMetadataOnly,
				EnforcedByHub: false,
			},
		}
		got := fetchTrajectoryCaptureNotice(context.Background(), f, time.Second)
		assert.Equal(t, telemetrycontract.TrajectoryCaptureNotice(*f.status), got)
	})

	t.Run("returns empty for policy off", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{
			configured: true,
			status: &telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy: telemetrycontract.TrajectoryCapturePolicyOff,
			},
		}
		assert.Empty(t, fetchTrajectoryCaptureNotice(context.Background(), f, time.Second))
	})

	t.Run("returns empty when experiment off (nil status)", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{configured: true, status: nil}
		assert.Empty(t, fetchTrajectoryCaptureNotice(context.Background(), f, time.Second))
	})

	t.Run("returns empty on error without failing", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{configured: true, err: errors.New("hub unreachable")}
		assert.Empty(t, fetchTrajectoryCaptureNotice(context.Background(), f, time.Second))
	})

	t.Run("respects timeout bound", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{
			configured: true,
			delay:      2 * time.Second,
			status: &telemetrycontract.TrajectoryCapturePolicyStatus{
				Policy: telemetrycontract.TrajectoryCapturePolicyFull,
			},
		}
		start := time.Now()
		got := fetchTrajectoryCaptureNotice(context.Background(), f, 50*time.Millisecond)
		assert.Empty(t, got)
		assert.Less(t, time.Since(start), 500*time.Millisecond)
	})
}

func TestInjectTmuxTrajectoryNotice(t *testing.T) {
	notice := telemetrycontract.TrajectoryCaptureNotice(telemetrycontract.TrajectoryCapturePolicyStatus{
		Policy:        telemetrycontract.TrajectoryCapturePolicyFull,
		EnforcedByHub: true,
	})

	t.Run("injects printf before harness inside tmux agent window", func(t *testing.T) {
		orig := []string{
			"sh", "-c",
			"tmux new-session -d -s scion -n agent sh -c 'claude --no-chrome; scion_hrc=$?; echo $scion_hrc > /tmp/scion-harness-exit-code' \\; attach-session -t scion",
		}
		got := injectTmuxTrajectoryNotice(orig, notice)
		require.Len(t, got, 3)
		assert.Equal(t, "sh", got[0])
		assert.Equal(t, "-c", got[1])
		assert.Contains(t, got[2], `tmux new-session -d -s scion -n agent sh -c 'printf "%s\n" "Notice: `+notice+`"; claude --no-chrome;`)
		// Original slice must not be mutated.
		assert.False(t, strings.Contains(orig[2], "Notice:"))
	})

	t.Run("leaves non-tmux command untouched", func(t *testing.T) {
		orig := []string{"claude", "--no-chrome"}
		got := injectTmuxTrajectoryNotice(orig, notice)
		assert.Equal(t, orig, got)
	})

	t.Run("leaves command untouched when notice is empty", func(t *testing.T) {
		orig := []string{"sh", "-c", "tmux new-session -d -s scion -n agent sh -c 'claude'"}
		got := injectTmuxTrajectoryNotice(orig, "")
		assert.Equal(t, orig, got)
	})
}

func TestMaybeInjectTmuxTrajectoryNotice_SkipsFetchOnNonInjectableShapes(t *testing.T) {
	fullStatus := &telemetrycontract.TrajectoryCapturePolicyStatus{
		Policy:        telemetrycontract.TrajectoryCapturePolicyFull,
		EnforcedByHub: false,
	}

	t.Run("Cloud Run-shaped /bin/sh command makes no Hub GET and stays unchanged", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{configured: true, status: fullStatus}
		orig := []string{
			"/bin/sh", "-c",
			"tmux new-session -d -s scion -n agent /bin/sh -c 'claude --no-chrome' \\; select-window -t scion:agent; while tmux has-session -t scion 2>/dev/null; do sleep 2; done",
		}
		got := maybeInjectTmuxTrajectoryNotice(context.Background(), f, orig, time.Second)
		assert.Equal(t, 0, f.calls, "must not call Hub GET for non-injectable Cloud Run command")
		assert.Equal(t, orig, got)
	})

	t.Run("K8s sh -c $SCION_START_CMD shape makes no Hub GET and stays unchanged", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{configured: true, status: fullStatus}
		orig := []string{"sh", "-c", `"$SCION_START_CMD"`}
		got := maybeInjectTmuxTrajectoryNotice(context.Background(), f, orig, time.Second)
		assert.Equal(t, 0, f.calls, "must not call Hub GET for K8s $SCION_START_CMD shape")
		assert.Equal(t, orig, got)
	})

	t.Run("Docker/Podman non-FUSE shape fetches once and injects notice", func(t *testing.T) {
		f := &stubTrajectoryPolicyFetcher{configured: true, status: fullStatus}
		orig := []string{
			"sh", "-c",
			"tmux new-session -d -s scion -n agent sh -c 'claude --no-chrome' \\; attach-session -t scion",
		}
		got := maybeInjectTmuxTrajectoryNotice(context.Background(), f, orig, time.Second)
		assert.Equal(t, 1, f.calls)
		assert.Contains(t, got[2], `printf "%s\n" "Notice: `+telemetrycontract.TrajectoryCaptureNotice(*fullStatus)+`"; claude --no-chrome`)
	})
}
