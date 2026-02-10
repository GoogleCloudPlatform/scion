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

package runtime

import (
	"context"
	"os"
	"os/exec"
	"runtime"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/k8s"
	"github.com/ptone/scion-agent/pkg/util"
)

// GetRuntime returns the appropriate Runtime implementation based on environment,
// agent configuration (if available via GetAgentSettings), and grove/global settings.
func GetRuntime(grovePath string, profileName string) Runtime {
	projectDir, _ := config.GetResolvedProjectDir(grovePath)
	s, _ := config.LoadSettings(projectDir)

	util.Debugf("GetRuntime: grovePath=%q, profileName=%q, projectDir=%q, hasSettings=%v", grovePath, profileName, projectDir, s != nil)

	var rtConfig config.RuntimeConfig
	var runtimeType string

	if s != nil {
		var err error
		var rtName string
		rtConfig, rtName, err = s.ResolveRuntime(profileName)
		if err != nil {
			util.Debugf("GetRuntime: ResolveRuntime failed: %v", err)
			// If profile resolution fails, we might be passed a direct runtime type
			// Fallback to legacy behavior for now if profileName matches a known type
			if profileName == "docker" || profileName == "kubernetes" || profileName == "k8s" || profileName == "container" || profileName == "remote" || profileName == "local" {
				runtimeType = profileName
				util.Debugf("GetRuntime: using profileName as runtimeType: %s", runtimeType)
			} else {
				// Final fallback to auto-detection
				runtimeType = "auto"
				util.Debugf("GetRuntime: fallback to auto-detection")
			}
		} else {
			runtimeType = rtName
			util.Debugf("GetRuntime: resolved runtime from settings: %s", runtimeType)
		}
	} else {
		runtimeType = "auto"
		util.Debugf("GetRuntime: no settings found, using auto-detection")
	}

	// Normalize runtime names
	if runtimeType == "remote" {
		runtimeType = "kubernetes"
	}

	if runtimeType == "local" || runtimeType == "auto" {
		util.Debugf("GetRuntime: auto-detecting for OS=%s", runtime.GOOS)
		if runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("container"); err == nil {
				runtimeType = "container"
				util.Debugf("GetRuntime: detected 'container' CLI on macOS")
			} else {
				runtimeType = "docker"
				util.Debugf("GetRuntime: 'container' CLI not found on macOS, using docker")
			}
		} else {
			runtimeType = "docker"
			util.Debugf("GetRuntime: non-macOS platform, using docker")
		}
	}

	if runtimeType == "remote" {
		runtimeType = "kubernetes"
	}

	util.Debugf("GetRuntime: final runtime type: %s", runtimeType)

	switch runtimeType {
	case "container":
		return NewAppleContainerRuntime()
	case "docker":
		dr := NewDockerRuntime()
		if rtConfig.Host != "" {
			dr.Host = rtConfig.Host
		}
		return dr
	case "kubernetes", "k8s":
		k8sClient, err := k8s.NewClient(os.Getenv("KUBECONFIG"))
		if err != nil {
			return &ErrorRuntime{Err: err}
		}
		rt := NewKubernetesRuntime(k8sClient)
		if rtConfig.Context != "" {
			// Need to support context switching in k8s client
		}
		if rtConfig.Namespace != "" {
			rt.DefaultNamespace = rtConfig.Namespace
		}
		if rtConfig.Sync != "" {
			rt.SyncMode = rtConfig.Sync
		} else {
			rt.SyncMode = "tar" // Implicit default
		}
		return rt
	}

	// Fallback should not be reached if logic is correct, but default to Docker
	return NewDockerRuntime()
}

type ErrorRuntime struct {
	Err error
}

func (e *ErrorRuntime) Name() string {
	return "error"
}

func (e *ErrorRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	return "", e.Err
}

func (e *ErrorRuntime) Stop(ctx context.Context, id string) error {
	return e.Err
}

func (e *ErrorRuntime) Delete(ctx context.Context, id string) error {
	return e.Err
}

func (e *ErrorRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	return nil, e.Err
}

func (e *ErrorRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	return "", e.Err
}

func (e *ErrorRuntime) Attach(ctx context.Context, id string) error {
	return e.Err
}

func (e *ErrorRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	return false, e.Err
}

func (e *ErrorRuntime) PullImage(ctx context.Context, image string) error {
	return e.Err
}

func (e *ErrorRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {
	return e.Err
}

func (e *ErrorRuntime) Exec(ctx context.Context, id string, cmd []string) (string, error) {
	return "", e.Err
}

func (e *ErrorRuntime) GetWorkspacePath(ctx context.Context, id string) (string, error) {
	return "", e.Err
}
