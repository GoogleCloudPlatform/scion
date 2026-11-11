package runtime

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
		{"DeleteResource", &fakeInstancesClient{getInstance: ciInstance(ciRunA)}, func(rt *CloudRunRuntime) error {
			return rt.DeleteResource(context.Background(), api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance, Namespace: ciParent, Name: id, UID: ciRunA})
		}},
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

// A handle DeleteResource refuses before any call is not this runtime's
// instance, so it keeps the exec key. The config is resolved first, so a
// forget deferred before the refusal checks would be recorded.
func TestCloudRunDeleteResource_RefusedHandleKeepsExecKey(t *testing.T) {
	const id = "agent-foo-0123456789"
	own := api.ResourceHandle{Kind: api.ResourceKindCloudRunInstance, Namespace: ciParent, Name: id, UID: ciRunA}

	tests := []struct {
		name   string
		handle func(h api.ResourceHandle) api.ResourceHandle
	}{
		{"wrong namespace", func(h api.ResourceHandle) api.ResourceHandle {
			h.Namespace = "projects/other-project/locations/us-central1"
			return h
		}},
		{"wrong kind", func(h api.ResourceHandle) api.ResourceHandle { h.Kind = "other-kind"; return h }},
		{"empty UID", func(h api.ResourceHandle) api.ResourceHandle { h.UID = ""; return h }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rt := newFakeCloudRunRuntime(t, &fakeInstancesClient{getInstance: ciInstance(ciRunA)})
			conn := &forgettingExecConnector{}
			rt.exec = conn
			if err := rt.resolveConfig(context.Background()); err != nil {
				t.Fatalf("resolveConfig: %v", err)
			}

			if err := rt.DeleteResource(context.Background(), tc.handle(own)); err == nil {
				t.Errorf("DeleteResource = nil, want an error")
			}
			if len(conn.forgotten) != 0 {
				t.Errorf("Forget calls = %q, want none", conn.forgotten)
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
