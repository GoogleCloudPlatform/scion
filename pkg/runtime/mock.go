package runtime

import (
	"context"

	"github.com/ptone/scion-agent/pkg/api"
)

type MockRuntime struct {
	RunFunc         func(ctx context.Context, config RunConfig) (string, error)
	StopFunc        func(ctx context.Context, id string) error
	DeleteFunc      func(ctx context.Context, id string) error
	ListFunc        func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error)
	GetLogsFunc     func(ctx context.Context, id string) (string, error)
	AttachFunc      func(ctx context.Context, id string) error
	ImageExistsFunc func(ctx context.Context, image string) (bool, error)
	SyncFunc        func(ctx context.Context, id string, direction SyncDirection) error
}

func (m *MockRuntime) Name() string {
	return "mock"
}

func (m *MockRuntime) Run(ctx context.Context, config RunConfig) (string, error) {
	if m.RunFunc != nil {
		return m.RunFunc(ctx, config)
	}
	return "mock-id", nil
}

func (m *MockRuntime) Stop(ctx context.Context, id string) error {
	if m.StopFunc != nil {
		return m.StopFunc(ctx, id)
	}
	return nil
}

func (m *MockRuntime) Delete(ctx context.Context, id string) error {
	if m.DeleteFunc != nil {
		return m.DeleteFunc(ctx, id)
	}
	return nil
}

func (m *MockRuntime) List(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
	if m.ListFunc != nil {
		return m.ListFunc(ctx, labelFilter)
	}
	return []api.AgentInfo{}, nil
}

func (m *MockRuntime) GetLogs(ctx context.Context, id string) (string, error) {
	if m.GetLogsFunc != nil {
		return m.GetLogsFunc(ctx, id)
	}
	return "mock logs", nil
}

func (m *MockRuntime) Attach(ctx context.Context, id string) error {
	if m.AttachFunc != nil {
		return m.AttachFunc(ctx, id)
	}
	return nil
}

func (m *MockRuntime) ImageExists(ctx context.Context, image string) (bool, error) {
	if m.ImageExistsFunc != nil {
		return m.ImageExistsFunc(ctx, image)
	}
	return true, nil
}

func (m *MockRuntime) PullImage(ctx context.Context, image string) error {
	return nil
}

func (m *MockRuntime) Sync(ctx context.Context, id string, direction SyncDirection) error {

	if m.SyncFunc != nil {

		return m.SyncFunc(ctx, id, direction)

	}

	return nil

}
