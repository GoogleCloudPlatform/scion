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

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

func TestStart_DurationBackstopGateAndDeadline(t *testing.T) {
	t.Run("enabled with positive max_duration stamps deadline with 12m total grace and echoes EffectiveRunDeadline", func(t *testing.T) {
		project, _ := envFixture(t, envSettingsV1)
		ctx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{
			Experiments: []string{experiments.AgentPolicyDurationBackstop},
		})
		before := time.Now().UTC()
		rc, info := startEnvCtxWithInfo(t, ctx, project, func(o *api.StartOptions) {
			o.InlineConfig = &api.ScionConfig{MaxDuration: "5m"}
		})
		after := time.Now().UTC()

		wantDuration := 5*time.Minute + api.DurationBackstopTotalGrace
		if rc.RunDeadlineDuration != wantDuration {
			t.Fatalf("RunDeadlineDuration = %v, want %v", rc.RunDeadlineDuration, wantDuration)
		}
		if rc.RunDeadline.IsZero() {
			t.Fatal("RunDeadline is zero, want non-zero UTC deadline")
		}
		minDL := before.Add(wantDuration).Truncate(time.Second)
		maxDL := after.Add(wantDuration).Add(time.Second)
		if rc.RunDeadline.Before(minDL) || rc.RunDeadline.After(maxDL) {
			t.Errorf("RunDeadline = %v, want in [%v, %v]", rc.RunDeadline, minDL, maxDL)
		}
		if want := rc.RunDeadline.UTC().Format(time.RFC3339); info.EffectiveRunDeadline != want {
			t.Errorf("EffectiveRunDeadline = %q, want %q", info.EffectiveRunDeadline, want)
		}
	})

	t.Run("slow startup (>2m, <12m) leaves backstop deadline after sciontool cooperative exit", func(t *testing.T) {
		project, _ := envFixture(t, envSettingsV1)
		ctx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{
			Experiments: []string{experiments.AgentPolicyDurationBackstop},
		})
		launchTime := time.Now().UTC()
		rc, _ := startEnvCtxWithInfo(t, ctx, project, func(o *api.StartOptions) {
			o.InlineConfig = &api.ScionConfig{MaxDuration: "2m"}
		})
		// Suppose image pull + clone + hooks take 4m (> 2m cooperative grace,
		// < 10m startup allowance); sciontool arms its 2m timer at launch+4m
		// and exits cooperatively at launch+6m. The broker backstop deadline
		// (launch + 2m + 12m = launch + 14m) must remain strictly after that.
		sciontoolWouldExit := launchTime.Add(4*time.Minute + 2*time.Minute)
		if !rc.RunDeadline.After(sciontoolWouldExit) {
			t.Fatalf("RunDeadline (%v) must be after sciontool cooperative exit (%v) for a 4m slow startup",
				rc.RunDeadline, sciontoolWouldExit)
		}
	})

	t.Run("disabled experiment leaves RunDeadline zero and EffectiveRunDeadline empty", func(t *testing.T) {
		project, _ := envFixture(t, envSettingsV1)
		ctx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{})
		rc, info := startEnvCtxWithInfo(t, ctx, project, func(o *api.StartOptions) {
			o.InlineConfig = &api.ScionConfig{MaxDuration: "5m"}
		})
		if !rc.RunDeadline.IsZero() || rc.RunDeadlineDuration != 0 {
			t.Errorf("expected zero deadline when experiment is off, got deadline=%v dur=%v", rc.RunDeadline, rc.RunDeadlineDuration)
		}
		if info.EffectiveRunDeadline != "" {
			t.Errorf("EffectiveRunDeadline = %q, want empty", info.EffectiveRunDeadline)
		}
	})

	t.Run("enabled experiment with unlimited, unset, or whitespace-padded max_duration leaves RunDeadline zero", func(t *testing.T) {
		// Whitespace-padded values like " 5m" are parsed strictly by raw
		// api.ParseDuration (no TrimSpace) to match sciontool and avoid a
		// deadline mismatch.
		for _, maxDur := range []string{"", "0", " 5m"} {
			project, _ := envFixture(t, envSettingsV1)
			ctx := api.ContextWithHubAgentDefaults(context.Background(), &api.HubAgentDefaults{
				Experiments: []string{experiments.AgentPolicyDurationBackstop},
			})
			rc, info := startEnvCtxWithInfo(t, ctx, project, func(o *api.StartOptions) {
				if maxDur != "" {
					o.InlineConfig = &api.ScionConfig{MaxDuration: maxDur}
				}
			})
			if !rc.RunDeadline.IsZero() || rc.RunDeadlineDuration != 0 || info.EffectiveRunDeadline != "" {
				t.Errorf("max_duration=%q: expected zero deadline, got deadline=%v dur=%v echo=%q",
					maxDur, rc.RunDeadline, rc.RunDeadlineDuration, info.EffectiveRunDeadline)
			}
		}
	})
}
