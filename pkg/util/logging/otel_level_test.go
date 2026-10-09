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

package logging

import (
	"context"
	"log/slog"
	"reflect"
	"sync"
	"testing"

	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging/loglevel"
)

// memProcessor is an in-memory sdklog.Processor that records the body of
// every exported log record. Enabled reports true for every severity, like
// the SDK's batch processor, so any filtering comes from the slog side.
type memProcessor struct {
	mu     sync.Mutex
	bodies []string
}

func (p *memProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (p *memProcessor) OnEmit(_ context.Context, r *sdklog.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bodies = append(p.bodies, r.Body().AsString())
	return nil
}

func (p *memProcessor) Shutdown(context.Context) error   { return nil }
func (p *memProcessor) ForceFlush(context.Context) error { return nil }

func (p *memProcessor) exported() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bodies...)
}

// TestSetupWithOTelFollowsLogLevel pins that the OTel sink follows
// SCION_LOG_LEVEL. Before the shared level filter, DEBUG records reached
// OTel even at the default info level (only stdout dropped them).
func TestSetupWithOTelFollowsLogLevel(t *testing.T) {
	tests := []struct {
		name     string
		logLevel string
		want     []string
	}{
		{
			name: "default info drops debug",
			want: []string{"probe-info", "auth-info"},
		},
		{
			name:     "debug exports debug",
			logLevel: "debug",
			want:     []string{"probe-debug", "probe-info", "auth-debug", "auth-info"},
		},
		{
			name:     "per-component debug only for that subsystem",
			logLevel: "info,hub.auth=debug",
			want:     []string{"probe-info", "auth-debug", "auth-info"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetLevelState(t)
			t.Setenv(loglevel.EnvLogLevel, tt.logLevel)
			t.Setenv("K_SERVICE", "")
			orig := slog.Default()
			t.Cleanup(func() { slog.SetDefault(orig) })

			proc := &memProcessor{}
			lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(proc))
			t.Cleanup(func() { _ = lp.Shutdown(context.Background()) })

			SetupWithOTel("test-component", "", false, false, lp)

			slog.Debug("probe-debug")
			slog.Info("probe-info")
			Subsystem("hub.auth").Debug("auth-debug")
			Subsystem("hub.auth").Info("auth-info")

			if got := proc.exported(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("exported = %v, want %v", got, tt.want)
			}
		})
	}
}
