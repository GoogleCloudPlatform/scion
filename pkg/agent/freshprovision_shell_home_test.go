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
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// TestManagerStart_FreshProvisionWipesShellHome verifies H1 (ptone/scion#8818):
// on a fresh-provision create over an existing agent directory, any populated
// home is wiped whenever either the requested OR the recorded harness config is
// "shell", so a delete+recreate (including after an offline-broker force-delete)
// can never inherit an execution container's $HOME credentials. Normal start /
// resume (FreshProvision=false) preserves the shell home, and non-shell fresh
// provision behaviour is unchanged.
func TestManagerStart_FreshProvisionWipesShellHome(t *testing.T) {
	seedCredential := func(t *testing.T, fx *freshProvisionManagerFixture, configJSON, credContent string) string {
		t.Helper()
		agentDir := filepath.Join(fx.projectScionDir, "agents", "existing-agent")
		writeFile(t, filepath.Join(agentDir, "scion-agent.json"), configJSON)
		credPath := filepath.Join(agentDir, "home", ".config", "gh", "hosts.yml")
		mkdirAll(t, filepath.Dir(credPath))
		writeFile(t, credPath, credContent)
		return credPath
	}

	t.Run("fresh provision of shell over seeded shell home wipes credentials", func(t *testing.T) {
		fx := newFreshProvisionManagerFixture(t)
		credPath := seedCredential(t, fx, `{"harness":"shell","harness_config":"shell"}`, "oauth_token: gho_secret\n")

		opts := fx.opts
		opts.FreshProvision = true
		opts.HarnessConfig = config.ShellHarnessConfigName
		if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Manager.Start(FreshProvision=true, shell) failed: %v", err)
		}
		if _, err := os.Stat(credPath); !os.IsNotExist(err) {
			t.Fatalf("expected credential file %s to be wiped on fresh provision, stat err=%v", credPath, err)
		}
	})

	t.Run("fresh provision of non-shell over seeded shell home wipes credentials", func(t *testing.T) {
		fx := newFreshProvisionManagerFixture(t)
		credPath := seedCredential(t, fx, `{"harness":"shell","harness_config":"shell"}`, "oauth_token: gho_secret\n")

		opts := fx.opts
		opts.FreshProvision = true
		opts.HarnessConfig = "test-harness"
		if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Manager.Start(FreshProvision=true, non-shell over shell) failed: %v", err)
		}
		if _, err := os.Stat(credPath); !os.IsNotExist(err) {
			t.Fatalf("expected credential file %s to be wiped when recorded config is shell, stat err=%v", credPath, err)
		}
	})

	t.Run("fresh provision of shell over seeded non-shell home wipes home", func(t *testing.T) {
		fx := newFreshProvisionManagerFixture(t)
		credPath := seedCredential(t, fx, `{"harness":"gemini","default_harness_config":"test-harness"}`, "oauth_token: gho_secret\n")

		opts := fx.opts
		opts.FreshProvision = true
		opts.HarnessConfig = config.ShellHarnessConfigName
		if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Manager.Start(FreshProvision=true, shell over non-shell) failed: %v", err)
		}
		if _, err := os.Stat(credPath); !os.IsNotExist(err) {
			t.Fatalf("expected file %s to be wiped when requested config is shell, stat err=%v", credPath, err)
		}
	})

	t.Run("normal start/resume without FreshProvision preserves shell home", func(t *testing.T) {
		fx := newFreshProvisionManagerFixture(t)
		credPath := seedCredential(t, fx, `{"harness":"shell","harness_config":"shell"}`, "oauth_token: gho_keep\n")

		opts := fx.opts
		opts.FreshProvision = false
		opts.HarnessConfig = config.ShellHarnessConfigName
		if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Manager.Start(FreshProvision=false, shell) failed: %v", err)
		}
		data, err := os.ReadFile(credPath)
		if err != nil {
			t.Fatalf("expected credential file %s to survive non-fresh resume: %v", credPath, err)
		}
		if string(data) != "oauth_token: gho_keep\n" {
			t.Fatalf("credential file content changed: got %q", string(data))
		}
	})

	t.Run("control: non-shell fresh provision preserves home", func(t *testing.T) {
		fx := newFreshProvisionManagerFixture(t)
		credPath := seedCredential(t, fx, `{"harness":"gemini","default_harness_config":"test-harness"}`, "non-shell-data\n")

		opts := fx.opts
		opts.FreshProvision = true
		if _, err := fx.mgr.Start(context.Background(), opts); err != nil {
			t.Fatalf("Manager.Start(FreshProvision=true, non-shell) failed: %v", err)
		}
		data, err := os.ReadFile(credPath)
		if err != nil {
			t.Fatalf("expected non-shell home file %s to remain unchanged: %v", credPath, err)
		}
		if string(data) != "non-shell-data\n" {
			t.Fatalf("non-shell home file content changed: got %q", string(data))
		}
	})
}
