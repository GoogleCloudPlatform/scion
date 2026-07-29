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

package cmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

// fakeBrokerEnvShadowWarner records whether the boot wiring actually invoked
// the check, and with which context.
type fakeBrokerEnvShadowWarner struct {
	calls  int
	gotCtx context.Context
	err    error
}

func (f *fakeBrokerEnvShadowWarner) WarnOutrankedBrokerEnvKeys(ctx context.Context) error {
	f.calls++
	f.gotCtx = ctx
	return f.err
}

// *hub.HTTPAgentDispatcher must remain assignable to the boot seam. If the
// method is renamed or its signature changes, this fails to compile rather
// than silently leaving the boot path calling nothing.
var _ brokerEnvShadowWarner = (*hub.HTTPAgentDispatcher)(nil)

func TestWarnShadowedBrokerEnv_InvokesTheCheck(t *testing.T) {
	f := &fakeBrokerEnvShadowWarner{}
	ctx := context.Background()

	warnShadowedBrokerEnv(ctx, f)

	if f.calls != 1 {
		t.Fatalf("WarnOutrankedBrokerEnvKeys called %d times, want exactly 1 (boot runs once)", f.calls)
	}
	if f.gotCtx != ctx {
		t.Errorf("check received a different context than the boot context it was handed")
	}
}

// A failing check must not be reported as though the check ran and found
// nothing. This is the same defect one level down from the one the check
// itself reports: a silent non-run that looks like a clean result.
func TestWarnShadowedBrokerEnv_FailureSaysDidNotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"store error", errors.New("listing hub-scoped env vars: connection refused")},
		{"context cancelled", context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := swapDefaultLogger(t, &buf)
			defer restore()

			warnShadowedBrokerEnv(context.Background(), &fakeBrokerEnvShadowWarner{err: tc.err})

			out := buf.String()
			if !strings.Contains(out, "DID NOT RUN") {
				t.Errorf("failure log must say the check DID NOT RUN, so an operator cannot read silence as 'nothing is shadowed'.\ngot: %s", out)
			}
			if !strings.Contains(out, tc.err.Error()) {
				t.Errorf("failure log must carry the underlying error.\ngot: %s", out)
			}
		})
	}
}

// Negative control for the two tests above: on the success path nothing is
// logged at all. Without this, a helper that logged DID NOT RUN unconditionally
// would pass both assertions above.
func TestWarnShadowedBrokerEnv_SuccessLogsNothing(t *testing.T) {
	var buf bytes.Buffer
	restore := swapDefaultLogger(t, &buf)
	defer restore()

	warnShadowedBrokerEnv(context.Background(), &fakeBrokerEnvShadowWarner{})

	if got := buf.String(); got != "" {
		t.Errorf("successful check must log nothing from the wiring; the method does its own logging.\ngot: %s", got)
	}
}

// TestBootPathCallsWarnShadowedBrokerEnv is a DRIFT GUARD OVER SOURCE TEXT. IT
// IS NOT A CORRECTNESS CHECK AND MUST NEVER BE COUNTED AS ONE.
//
// It exists because of a gap the unit tests above genuinely cannot close.
// runServerForeground is not callable from a test, so nothing else in the suite
// fails if the single call site in step 14 is deleted: the method would still
// work, the helper would still be tested, and the warning would never be
// emitted by a real hub. That is precisely the "looks done in the diff, silent
// at runtime" failure this whole change is about, so it gets a guard even
// though the guard is a crude one.
//
// What it does NOT cover: that step 14 executes, that enableHub is true, that
// the dispatcher passed is the live one, or that the store behind it is
// reachable. Nor can it tell a live call from one that is present at statement
// position but unreachable — inside an if false, a disabled branch, or a
// build-tagged file. The text being there, in statement position, is the whole
// of what it establishes.
//
// It fails when the line is deleted AND when it is commented out. An earlier
// version of this guard used strings.Contains and claimed "it only fails when
// the line disappears"; that claim was too generous in one direction and too
// modest in the other. strings.Contains is satisfied by a leading "// ", so
// commenting the call out left the whole suite green — and commenting out is
// the commonest way a call gets disabled and is indistinguishable from
// deletion in a diff. The pattern below is anchored to statement position
// precisely to close that case.
var bootCallSite = regexp.MustCompile(`(?m)^[\t ]*warnShadowedBrokerEnv\(ctx, dispatcher\)[\t ]*$`)

func TestBootPathCallsWarnShadowedBrokerEnv(t *testing.T) {
	src, err := os.ReadFile("server_foreground.go")
	if err != nil {
		t.Fatalf("reading boot source: %v", err)
	}
	if !bootCallSite.Match(src) {
		t.Fatal("boot path no longer calls warnShadowedBrokerEnv(ctx, dispatcher) at statement " +
			"position — deleted, or commented out. The broker env shadow warning is required; " +
			"without this call it is dead code that a diff still makes look shipped.")
	}
}

func swapDefaultLogger(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }
}
