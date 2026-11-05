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

package tmuxdeliver_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/tmuxdeliver"
)

// TestCheckKey: the allowlist is deny by default and matched exactly.
func TestCheckKey(t *testing.T) {
	for _, k := range []string{"Enter", "Escape", "C-c"} {
		if !tmuxdeliver.CheckKey(k) {
			t.Errorf("CheckKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{
		// Left out on purpose: C-d can end a harness at an empty prompt; the
		// others have no shipped user.
		"C-d", "Tab", "C-j", "C-m", "C-u", "C-z", "C-\\",
		"", "enter", "ENTER", "Enter ", " Enter", "C-C", "c-c", "M-x", "F1", "q", "y",
		"-l", "-t", "-X", "Enter;", "Enter\n", "Enter\x00", "send-keys", "C-c C-c",
	} {
		if tmuxdeliver.CheckKey(k) {
			t.Errorf("CheckKey(%q) = true, want false", k)
		}
	}
}

func fieldOf(t *testing.T, err error) string {
	t.Helper()
	var be *tmuxdeliver.BoundsError
	if !errors.As(err, &be) {
		t.Fatalf("error %v is not a *BoundsError", err)
	}
	return be.Field
}

func goodSettings() tmuxdeliver.Settings {
	return tmuxdeliver.Settings{
		SubmitKeys:       []string{"Enter"},
		InterruptKeys:    []string{"Escape", "C-c"},
		ExtraSubmits:     2,
		ExtraSubmitDelay: 300 * time.Millisecond,
		ReadyStrategy:    state.ReadyStrategyHook,
		ReadySettle:      1500 * time.Millisecond,
		ReadyTimeout:     90 * time.Second,
	}
}

// TestCheckBounds names the first field outside the route bounds and never
// clamps.
func TestCheckBounds(t *testing.T) {
	if err := tmuxdeliver.CheckBounds(goodSettings()); err != nil {
		t.Fatalf("CheckBounds(good) = %v", err)
	}
	cases := []struct {
		name  string
		mod   func(*tmuxdeliver.Settings)
		field string
	}{
		{"no submit keys", func(s *tmuxdeliver.Settings) { s.SubmitKeys = nil }, tmuxdeliver.FieldSubmitKeys},
		{"unknown submit key", func(s *tmuxdeliver.Settings) { s.SubmitKeys = []string{"M-Enter"} }, tmuxdeliver.FieldSubmitKeys},
		{"five submit keys", func(s *tmuxdeliver.Settings) { s.SubmitKeys = []string{"Enter", "Enter", "Enter", "Enter", "Enter"} }, tmuxdeliver.FieldSubmitKeys},
		{"no interrupt keys", func(s *tmuxdeliver.Settings) { s.InterruptKeys = nil }, tmuxdeliver.FieldInterruptKeys},
		{"five interrupt keys", func(s *tmuxdeliver.Settings) { s.InterruptKeys = []string{"C-c", "C-c", "C-c", "C-c", "C-c"} }, tmuxdeliver.FieldInterruptKeys},
		{"flag as interrupt key", func(s *tmuxdeliver.Settings) { s.InterruptKeys = []string{"-l"} }, tmuxdeliver.FieldInterruptKeys},
		{"four extra submits", func(s *tmuxdeliver.Settings) { s.ExtraSubmits = 4 }, tmuxdeliver.FieldExtraSubmits},
		{"negative extra submits", func(s *tmuxdeliver.Settings) { s.ExtraSubmits = -1 }, tmuxdeliver.FieldExtraSubmits},
		{"3s extra submit delay", func(s *tmuxdeliver.Settings) { s.ExtraSubmitDelay = 3 * time.Second }, tmuxdeliver.FieldExtraSubmitDelay},
		{"unknown strategy", func(s *tmuxdeliver.Settings) { s.ReadyStrategy = "settle" }, tmuxdeliver.FieldReadyStrategy},
		{"11s settle", func(s *tmuxdeliver.Settings) { s.ReadySettle = 11 * time.Second }, tmuxdeliver.FieldReadySettle},
		{"91s timeout", func(s *tmuxdeliver.Settings) { s.ReadyTimeout = 91 * time.Second }, tmuxdeliver.FieldReadyTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodSettings()
			tc.mod(&s)
			err := tmuxdeliver.CheckBounds(s)
			if err == nil {
				t.Fatalf("CheckBounds(%s) = nil, want a bounds error", s)
			}
			if got := fieldOf(t, err); got != tc.field {
				t.Errorf("field = %q, want %q", got, tc.field)
			}
			if err.Error() == "" || len(err.Error()) > 120 {
				t.Errorf("error text %q: want a short field-only message", err.Error())
			}
		})
	}
}

// TestCheckRoute applies the route bounds to a request.
func TestCheckRoute(t *testing.T) {
	ok := tmuxdeliver.Request{Op: tmuxdeliver.OpInterruptPaste, Text: "x", InterruptKeys: []string{"C-c"}, SubmitKeys: []string{"Enter"}, ExtraSubmits: 3, ExtraSubmitDelay: 2 * time.Second, BufferName: "b"}
	if err := tmuxdeliver.CheckRoute(ok); err != nil {
		t.Fatalf("CheckRoute(ok) = %v", err)
	}
	for name, mod := range map[string]func(*tmuxdeliver.Request){
		tmuxdeliver.FieldSubmitKeys:       func(r *tmuxdeliver.Request) { r.SubmitKeys = []string{"KPEnter"} },
		tmuxdeliver.FieldInterruptKeys:    func(r *tmuxdeliver.Request) { r.InterruptKeys = []string{"C-z"} },
		tmuxdeliver.FieldExtraSubmits:     func(r *tmuxdeliver.Request) { r.ExtraSubmits = 4 },
		tmuxdeliver.FieldExtraSubmitDelay: func(r *tmuxdeliver.Request) { r.ExtraSubmitDelay = 2*time.Second + time.Millisecond },
	} {
		r := ok
		mod(&r)
		err := tmuxdeliver.CheckRoute(r)
		if err == nil {
			t.Errorf("%s: CheckRoute = nil, want a bounds error", name)
			continue
		}
		if got := fieldOf(t, err); got != name {
			t.Errorf("field = %q, want %q", got, name)
		}
	}
}

// harnessConfig is the part of a shipped harnesses/*/config.yaml the route
// bounds cover.
type harnessConfig struct {
	InterruptKey      string   `yaml:"interrupt_key"`
	InterruptSequence []string `yaml:"interrupt_sequence"`
	Ready             struct {
		Strategy string `yaml:"strategy"`
		Settle   string `yaml:"settle"`
		Timeout  string `yaml:"timeout"`
	} `yaml:"ready"`
	Delivery struct {
		SubmitKeys       []string `yaml:"submit_keys"`
		ExtraSubmits     *int     `yaml:"extra_submits"`
		ExtraSubmitDelay string   `yaml:"extra_submit_delay"`
	} `yaml:"delivery"`
}

func parseDur(t *testing.T, file, field, v string) time.Duration {
	t.Helper()
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		t.Fatalf("%s: %s %q: %v", file, field, v, err)
	}
	return d
}

// TestShippedHarnessesFitRouteBounds: every shipped harness config fits the
// route bounds, so the route serves shipped harnesses (design v2.22 §1.7). A
// new harness or a config change outside the bounds fails here, not as a
// silent fallback to exec.
func TestShippedHarnessesFitRouteBounds(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "harnesses", "*", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no harnesses/*/config.yaml found")
	}
	for _, f := range files {
		t.Run(filepath.Base(filepath.Dir(f)), func(t *testing.T) {
			data, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var hc harnessConfig
			if err := yaml.Unmarshal(data, &hc); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			s := tmuxdeliver.Settings{
				SubmitKeys:       hc.Delivery.SubmitKeys,
				InterruptKeys:    hc.InterruptSequence,
				ExtraSubmits:     2,
				ExtraSubmitDelay: 300 * time.Millisecond,
				ReadyStrategy:    hc.Ready.Strategy,
				ReadySettle:      parseDur(t, f, "ready.settle", hc.Ready.Settle),
				ReadyTimeout:     parseDur(t, f, "ready.timeout", hc.Ready.Timeout),
			}
			if len(s.SubmitKeys) == 0 {
				s.SubmitKeys = []string{"Enter"}
			}
			if len(s.InterruptKeys) == 0 {
				key := hc.InterruptKey
				if key == "" {
					key = "C-c"
				}
				s.InterruptKeys = []string{key}
			}
			if hc.Delivery.ExtraSubmits != nil {
				s.ExtraSubmits = *hc.Delivery.ExtraSubmits
			}
			if hc.Delivery.ExtraSubmitDelay != "" {
				s.ExtraSubmitDelay = parseDur(t, f, "delivery.extra_submit_delay", hc.Delivery.ExtraSubmitDelay)
			}
			if s.ReadyStrategy == "" {
				s.ReadyStrategy = state.ReadyStrategyDelay
			}
			if err := tmuxdeliver.CheckBounds(s); err != nil {
				t.Errorf("%s does not fit the route bounds (%s): %v", f, s, err)
			}
		})
	}
}
