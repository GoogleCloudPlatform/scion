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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// The agent runtime's startup check of the Kubernetes placement defaults
// (ptone/scion#5085). Test IDs are from the placement design's catalogue.

type placementLog struct{ lines []string }

func (l *placementLog) logf(format string, args ...interface{}) {
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *placementLog) String() string { return strings.Join(l.lines, "\n") }

// placementSiblings has an explicit-context entry and an entry without a
// context that resolves, on a host whose current context is ctx-a, to the
// same cluster and namespace, with different node selectors.
func placementSiblings() *config.VersionedSettings {
	return &config.VersionedSettings{Runtimes: map[string]config.V1RuntimeConfig{
		"gke":          {Type: "kubernetes", Context: "ctx-a", Namespace: "agents", NodeSelector: map[string]string{"pool": "a"}},
		"gke-implicit": {Type: "kubernetes", Namespace: "agents", NodeSelector: map[string]string{"pool": "b"}},
		"elsewhere":    {Type: "kubernetes", Context: "ctx-b", Namespace: "agents", Tolerations: []api.K8sToleration{{Key: "k", Operator: "Exists"}}},
	}}
}

// allConfigured treats every runtime entry as configured.
func allConfigured() (map[string]bool, bool) { return nil, false }

func hostOnCtxA() (string, config.KubeConnection, error) {
	return "ctx-a", config.KubeConnKubeconfig, nil
}

// G1b (startup part): resolved siblings that disagree are logged as an
// error naming both. G5: the check has no error or exit path, so startup
// continues (starts on that identity are refused by the agent runtime
// server; see pkg/runtimebroker); in a process that does not run the agent
// runtime the check does not run and logs nothing.
func TestPlacementSiblingsStartup_G1b_G5(t *testing.T) {
	var log placementLog
	logPlacementSiblingsStartup(true, func() (*config.VersionedSettings, error) { return placementSiblings(), nil },
		allConfigured, hostOnCtxA, "agents", log.logf)
	out := log.String()
	if !strings.Contains(out, "Error: Kubernetes placement: runtimes.gke: runtime entries gke, gke-implicit target the same cluster and namespace with different placement") {
		t.Errorf("want an error naming both entries; log:\n%s", out)
	}
	if strings.Contains(out, "elsewhere") {
		t.Errorf("an entry for another cluster is not a sibling; log:\n%s", out)
	}

	// Hub-only process: nothing is loaded or logged.
	var hubOnly placementLog
	loaded := false
	logPlacementSiblingsStartup(false, func() (*config.VersionedSettings, error) {
		loaded = true
		return placementSiblings(), nil
	}, allConfigured, hostOnCtxA, "agents", hubOnly.logf)
	if loaded || len(hubOnly.lines) != 0 {
		t.Errorf("hub-only process: loaded=%v, log %q; want neither", loaded, hubOnly.lines)
	}
}

// Without any placement configured the check resolves nothing and logs
// nothing, even when the host's cluster identity cannot be resolved.
func TestPlacementSiblingsStartup_NoPlacementIsQuiet(t *testing.T) {
	var log placementLog
	called := false
	logPlacementSiblingsStartup(true, func() (*config.VersionedSettings, error) {
		return &config.VersionedSettings{Runtimes: map[string]config.V1RuntimeConfig{
			"kubernetes": {Type: "kubernetes"},
			"gke":        {Type: "kubernetes", Context: "ctx-a"},
		}}, nil
	}, allConfigured, func() (string, config.KubeConnection, error) {
		called = true
		return "", config.KubeConnUnknown, errors.New("no kubeconfig")
	}, "agents", log.logf)
	if called || len(log.lines) != 0 {
		t.Errorf("no placement configured: identity read=%v, log %q; want neither", called, log.lines)
	}
}

// Only configured runtime entries take part: an entry that exists only in
// the embedded defaults (here "kubernetes", no context, no placement) is
// not a sibling of a configured entry for the same identity.
func TestPlacementSiblingsStartup_DefaultsOnlyEntryExcluded(t *testing.T) {
	settings := &config.VersionedSettings{Runtimes: map[string]config.V1RuntimeConfig{
		"kubernetes": {Type: "kubernetes"},
		"incluster":  {Type: "kubernetes", NodeSelector: map[string]string{"pool": "agents"}},
	}}
	inCluster := func() (string, config.KubeConnection, error) { return "", config.KubeConnInCluster, nil }
	var log placementLog
	logPlacementSiblingsStartup(true, func() (*config.VersionedSettings, error) { return settings, nil },
		func() (map[string]bool, bool) { return map[string]bool{"incluster": true}, true },
		inCluster, "agents", log.logf)
	if len(log.lines) != 0 {
		t.Errorf("defaults-only entry reported: %q", log.lines)
	}
	// When configured entries cannot be told apart, every entry takes part.
	var all placementLog
	logPlacementSiblingsStartup(true, func() (*config.VersionedSettings, error) { return settings, nil },
		allConfigured, inCluster, "agents", all.logf)
	if !strings.Contains(all.String(), "runtime entries incluster, kubernetes target the same cluster and namespace") {
		t.Errorf("fail closed: want the disagreement; log:\n%s", all.String())
	}
}

// An entry without a context whose cluster cannot be resolved is grouped
// with every entry in its namespace (fail closed), with a warning.
func TestPlacementSiblingsStartup_UnresolvedHost(t *testing.T) {
	var log placementLog
	logPlacementSiblingsStartup(true, func() (*config.VersionedSettings, error) { return placementSiblings(), nil },
		allConfigured, func() (string, config.KubeConnection, error) {
			return "", config.KubeConnUnknown, errors.New("no kubeconfig")
		},
		"agents", log.logf)
	out := log.String()
	if !strings.Contains(out, "Warning: Kubernetes placement: runtimes.gke-implicit: cluster identity could not be resolved") {
		t.Errorf("want the unresolved-identity warning; log:\n%s", out)
	}
	if !strings.Contains(out, "runtime entries elsewhere, gke-implicit target the same cluster and namespace") &&
		!strings.Contains(out, "runtime entries gke, gke-implicit target the same cluster and namespace") {
		t.Errorf("want the fail-closed grouping to report a disagreement; log:\n%s", out)
	}
}

// checkPlacementSiblingsAtStartup reads the global settings and the host's
// kubeconfig, and is gated on the agent runtime being enabled.
func TestCheckPlacementSiblingsAtStartup(t *testing.T) {
	old := config.GetGlobalSettingsOverlay()
	t.Cleanup(func() { config.SetGlobalSettingsOverlay(old) })
	config.SetGlobalSettingsOverlay(nil)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("POD_NAMESPACE", "agents")
	t.Setenv("SCION_K8S_NAMESPACE", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(home); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.MkdirAll(filepath.Join(home, ".scion"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".scion", "settings.yaml"), []byte(`schema_version: "1"
runtimes:
  gke:
    type: kubernetes
    context: ctx-a
    namespace: agents
    node_selector:
      pool: a
  gke-implicit:
    type: kubernetes
    namespace: agents
    node_selector:
      pool: b
`), 0o644); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: ctx-a
current-context: ctx-a
users:
- name: u
  user:
    token: t
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	cfg := &config.GlobalConfig{}
	cfg.RuntimeBroker.Enabled = true
	var log placementLog
	checkPlacementSiblingsAtStartup(cfg, log.logf)
	if !strings.Contains(log.String(), "runtime entries gke, gke-implicit target the same cluster and namespace with different placement") {
		t.Errorf("want the disagreement logged; log:\n%s", log.String())
	}

	cfg.RuntimeBroker.Enabled = false
	var hubOnly placementLog
	checkPlacementSiblingsAtStartup(cfg, hubOnly.logf)
	if len(hubOnly.lines) != 0 {
		t.Errorf("hub-only process logged %q", hubOnly.lines)
	}
}
