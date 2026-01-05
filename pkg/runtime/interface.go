package runtime

import (
	"context"

	"github.com/ptone/scion-agent/pkg/api"
)

type RunConfig struct {
	Name         string
	Template     string
	UnixUsername string
	Image        string
	HomeDir      string
	Workspace    string
	RepoRoot     string
	Env          []string
	Volumes      []api.VolumeMount
	Labels       map[string]string
	Annotations  map[string]string
	Auth         api.AuthConfig
	Harness      api.Harness
	UseTmux      bool
	Task         string
	CommandArgs  []string
	Resume       bool
}

type Runtime interface {
	Name() string
	Run(ctx context.Context, config RunConfig) (string, error)
	Stop(ctx context.Context, id string) error
	Delete(ctx context.Context, id string) error
	List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error)
	GetLogs(ctx context.Context, id string) (string, error)
	Attach(ctx context.Context, id string) error
	ImageExists(ctx context.Context, image string) (bool, error)
	PullImage(ctx context.Context, image string) error
	Sync(ctx context.Context, id string, direction SyncDirection) error
}

type SyncDirection string

const (
	SyncTo          SyncDirection = "to"
	SyncFrom        SyncDirection = "from"
	SyncUnspecified SyncDirection = ""
)
