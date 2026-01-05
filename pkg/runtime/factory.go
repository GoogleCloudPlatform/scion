package runtime

import (
	"context"
	"os"
	"os/exec"
	"runtime"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/k8s"
)

// GetRuntime returns the appropriate Runtime implementation based on environment,
// agent configuration (if available via GetAgentSettings), and grove/global settings.
func GetRuntime(grovePath string, profileName string) Runtime {
	projectDir, _ := config.GetResolvedProjectDir(grovePath)
	s, _ := config.LoadSettings(projectDir)

	var rtConfig config.RuntimeConfig
	var runtimeType string

	if s != nil {
		var err error
		var rtName string
		rtConfig, rtName, err = s.ResolveRuntime(profileName)
		if err != nil {
			// If profile resolution fails, we might be passed a direct runtime type
			// Fallback to legacy behavior for now if profileName matches a known type
			if profileName == "docker" || profileName == "kubernetes" || profileName == "k8s" || profileName == "container" || profileName == "remote" || profileName == "local" {
				runtimeType = profileName
			} else {
				// Final fallback to auto-detection
				runtimeType = "auto"
			}
		} else {
			runtimeType = rtName
		}
	} else {
		runtimeType = "auto"
	}

	// Normalize runtime names
	if runtimeType == "remote" {
		runtimeType = "kubernetes"
	}

	if runtimeType == "local" || runtimeType == "auto" {
		if runtime.GOOS == "darwin" {
			if _, err := exec.LookPath("container"); err == nil {
				runtimeType = "container"
			} else {
				runtimeType = "docker"
			}
		} else {
			runtimeType = "docker"
		}
	}

	if runtimeType == "remote" {
		runtimeType = "kubernetes"
	}

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
