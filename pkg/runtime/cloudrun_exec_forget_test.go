package runtime

import (
	"context"
	"errors"
	"io"
	"testing"
)

// forgettingExecConnector records Forget calls; its exec methods are unused.
type forgettingExecConnector struct {
	forgotten []string
}

func (f *forgettingExecConnector) Connect(context.Context, string, string, string) error {
	return errors.New("not implemented")
}

func (f *forgettingExecConnector) Exec(context.Context, string, string, string, []string) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (f *forgettingExecConnector) ExecWithStdin(context.Context, string, string, string, []string, io.Reader) ([]byte, error) {
	return nil, errors.New("not implemented")
}

func (f *forgettingExecConnector) Forget(project, location, instanceName string) {
	f.forgotten = append(f.forgotten, project+"/"+location+"/"+instanceName)
}

func TestCloudRunTeardown_ForgetsExecKey(t *testing.T) {
	const id = "agent-foo-0123456789"
	const want = "test-project/us-central1/" + id

	tests := []struct {
		name string
		fake *fakeInstancesClient
		call func(*CloudRunRuntime) error
	}{
		{"Stop", &fakeInstancesClient{}, func(rt *CloudRunRuntime) error { return rt.Stop(context.Background(), RunRef{ID: id}) }},
		{"Delete", &fakeInstancesClient{}, func(rt *CloudRunRuntime) error { return rt.Delete(context.Background(), RunRef{ID: id}) }},
		{"Stop error", &fakeInstancesClient{stopErr: errors.New("boom")}, func(rt *CloudRunRuntime) error { return rt.Stop(context.Background(), RunRef{ID: id}) }},
		{"Delete error", &fakeInstancesClient{deleteErr: errors.New("boom")}, func(rt *CloudRunRuntime) error { return rt.Delete(context.Background(), RunRef{ID: id}) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeCloudRunRuntime(t, tc.fake)
			conn := &forgettingExecConnector{}
			rt.exec = conn

			_ = tc.call(rt)

			if len(conn.forgotten) != 1 || conn.forgotten[0] != want {
				t.Errorf("Forget calls = %q, want [%q]", conn.forgotten, want)
			}
		})
	}
}

func TestCloudRunForgetExecKey_SkippedUntilConfigResolved(t *testing.T) {
	rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{})
	conn := &forgettingExecConnector{}
	rt.exec = conn

	rt.forgetExecKey("agent-1")
	if len(conn.forgotten) != 0 {
		t.Fatalf("Forget calls before the config is resolved = %q, want none", conn.forgotten)
	}

	if err := rt.resolveConfig(context.Background()); err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	rt.forgetExecKey("agent-1")
	if want := "test-project/us-central1/agent-1"; len(conn.forgotten) != 1 || conn.forgotten[0] != want {
		t.Errorf("Forget calls = %q, want [%q]", conn.forgotten, want)
	}
}
