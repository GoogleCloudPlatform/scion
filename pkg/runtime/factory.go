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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// GetRuntime returns the appropriate Runtime implementation based on environment,
// agent configuration (if available via GetAgentSettings), and grove/global settings.
func GetRuntime(grovePath string, profileName string) Runtime {
	projectDir, _ := config.GetResolvedProjectDir(grovePath)
	vs, warnings, _ := config.LoadEffectiveSettings(projectDir)
	config.PrintDeprecationWarnings(warnings)

	util.Debugf("GetRuntime: grovePath=%q, profileName=%q, projectDir=%q, hasSettings=%v", grovePath, profileName, projectDir, vs != nil)

	var rtConfig config.V1RuntimeConfig
	var runtimeType string

	if vs != nil {
		var err error
		rtConfig, runtimeType, err = vs.ResolveRuntime(profileName)
		if err != nil {
			util.Debugf("GetRuntime: ResolveRuntime failed: %v", err)
			// If profile resolution fails, we might be passed a direct runtime type
			// Fallback to legacy behavior for now if profileName matches a known type
			if profileName == "docker" || profileName == "podman" || profileName == "kubernetes" || profileName == "k8s" || profileName == "container" || profileName == "remote" || profileName == "local" {
				runtimeType = profileName
				util.Debugf("GetRuntime: using profileName as runtimeType: %s", runtimeType)
			} else {
				// Final fallback to auto-detection
				runtimeType = "auto"
				util.Debugf("GetRuntime: fallback to auto-detection")
			}
		} else {
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
			// On Linux, prefer podman over docker when both are available
			if _, err := exec.LookPath("podman"); err == nil {
				runtimeType = "podman"
				util.Debugf("GetRuntime: detected 'podman' on Linux")
			} else {
				runtimeType = "docker"
				util.Debugf("GetRuntime: 'podman' not found on Linux, using docker")
			}
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
	case "podman":
		pr := NewPodmanRuntime()
		if rtConfig.Host != "" {
			if p, ok := pr.(*PodmanRuntime); ok {
				p.Host = rtConfig.Host
			}
		}
		return pr
	case "kubernetes", "k8s":
		k8sClient, err := k8s.NewClientWithContext(os.Getenv("KUBECONFIG"), rtConfig.Context)
		if err != nil {
			return &ErrorRuntime{Err: err}
		}
		if err := k8sClient.Verify(); err != nil {
			return &ErrorRuntime{Err: err}
		}
		rt := NewKubernetesRuntime(k8sClient)
		if rtConfig.Namespace != "" {
			rt.DefaultNamespace = rtConfig.Namespace
		}
		rt.GKEMode = rtConfig.GKE
		if !rt.GKEMode && k8sClient.IsGKE() {
			rt.GKEAutoDetected = true
			util.Debugf("GetRuntime: auto-detected GKE cluster, enabling Autopilot scheduling tolerance")
		}
		rt.ListAllNamespaces = rtConfig.ListAllNamespaces
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

func (e *ErrorRuntime) ExecUser() string {
	return "scion"
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

func (e *ErrorRuntime) GetLogsSince(ctx context.Context, id string, since time.Time) (string, error) {
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
