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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// brokerStateDirSetting names the setting in errors and warnings.
const brokerStateDirSetting = "server.broker.state_dir"

// brokerStateRoot resolves server.broker.state_dir (setting) to the base
// directory of the Runtime Broker's state. An empty setting returns "",
// meaning the default (~/.scion/runtime-broker-state, see
// runtimebroker.DefaultStateDir). A leading "~/" (or a bare "~") is the
// user's home directory. Any other value must be an absolute path.
func brokerStateRoot(setting string) (string, error) {
	setting = strings.TrimSpace(setting)
	if setting == "" {
		return "", nil
	}
	if setting == "~" || strings.HasPrefix(setting, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%s %q: resolving the home directory: %w", brokerStateDirSetting, setting, err)
		}
		return filepath.Join(home, strings.TrimPrefix(setting, "~")), nil
	}
	if !filepath.IsAbs(setting) {
		return "", fmt.Errorf("%s %q must be an absolute path or start with ~/", brokerStateDirSetting, setting)
	}
	return filepath.Clean(setting), nil
}

// brokerStateDir is the legacy Runtime Broker's runtimebroker.ServerConfig
// StateDir for brokerID: <root>/<broker-id> ("default" when the ID is
// empty, as runtimebroker.DefaultStateDir does), or "" when the setting is
// empty, which leaves the broker on its default directory.
func brokerStateDir(setting, brokerID string) (string, error) {
	root, err := brokerStateRoot(setting)
	if err != nil || root == "" {
		return "", err
	}
	if brokerID == "" {
		brokerID = "default"
	}
	return filepath.Join(root, brokerID), nil
}

// validateBrokerStateDir is the server preflight check of
// server.broker.state_dir: it rejects a value brokerStateRoot cannot
// resolve, before any server starts. It applies with or without the Hub.
func validateBrokerStateDir(cfg *config.GlobalConfig) error {
	if cfg == nil {
		return nil
	}
	_, err := brokerStateRoot(cfg.RuntimeBroker.StateDir)
	return err
}

// flatInstancesStateDirWarning is the startup warning when
// server.broker.state_dir is set on a process that hosts flat Runtime
// Broker instances, which keep their default state directories; "" when
// there is nothing to warn about.
func flatInstancesStateDirWarning(cfg *config.GlobalConfig) string {
	if cfg == nil || strings.TrimSpace(cfg.RuntimeBroker.StateDir) == "" {
		return ""
	}
	return fmt.Sprintf("%s is set but this process hosts flat Runtime Broker instances (server.broker.instances); they keep their default state directories (~/.scion/runtime-broker-state/<broker-id>)", brokerStateDirSetting)
}

// configuredBrokerStateRoot is the base directory of the Runtime Broker's
// state that 'runtime-broker deregister --purge-local' cleans: the
// configured server.broker.state_dir, or the default under scionHome when
// it is unset. It reads the global settings and environment of the shell
// that runs it. When the settings cannot be loaded or the value is invalid
// it returns the default root together with a non-nil error: the default
// is only a guess, which may be listed but must not be purged (fail
// closed; see cleanupAfterDeregister).
func configuredBrokerStateRoot(scionHome string) (string, error) {
	def := filepath.Join(scionHome, "runtime-broker-state")
	cfg, err := loadBrokerStateConfig("")
	if err != nil {
		return def, fmt.Errorf("%s could not be read: loading the settings: %w", brokerStateDirSetting, err)
	}
	if cfg == nil {
		return def, fmt.Errorf("%s could not be read: no configuration loaded", brokerStateDirSetting)
	}
	root, err := brokerStateRoot(cfg.RuntimeBroker.StateDir)
	if err != nil {
		return def, err
	}
	if root == "" {
		return def, nil
	}
	return root, nil
}

// loadBrokerStateConfig loads the global configuration the way the server
// does; a test seam.
var loadBrokerStateConfig = config.LoadGlobalConfig
