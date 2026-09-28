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

package fswatcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// fakeContainer describes a container served by the fake Docker daemon used
// in these tests.
type fakeContainer struct {
	id     string
	labels map[string]string
	mounts []container.MountPoint
}

// newFakeDockerClient starts a fake Docker daemon that serves ContainerList
// and ContainerInspect from the given fixtures, and returns a client pointed
// at it along with the label filter observed on the last list request.
func newFakeDockerClient(t *testing.T, containers []fakeContainer) (*client.Client, func() string) {
	t.Helper()

	byID := make(map[string]fakeContainer, len(containers))
	for _, c := range containers {
		byID[c.id] = c
	}

	var mu sync.Mutex
	var lastLabelFilter string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			args, err := filters.FromJSON(r.URL.Query().Get("filters"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			labelFilters := args.Get("label")
			if len(labelFilters) > 0 {
				mu.Lock()
				lastLabelFilter = labelFilters[0]
				mu.Unlock()
			}

			var matched []container.Summary
			for _, c := range containers {
				if len(labelFilters) == 0 || matchesAllLabels(c.labels, labelFilters) {
					matched = append(matched, container.Summary{ID: c.id, Labels: c.labels})
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(matched)

		case strings.Contains(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/json"):
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/json"), "/")
			id := parts[len(parts)-1]
			c, ok := byID[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			resp := container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{ID: c.id},
				Mounts:            c.mounts,
				Config:            &container.Config{Labels: c.labels},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	cli, err := client.NewClientWithOpts(
		client.WithHost(srv.URL),
		client.WithHTTPClient(srv.Client()),
		client.WithVersion("1.44"),
	)
	if err != nil {
		t.Fatalf("creating fake docker client: %v", err)
	}

	return cli, func() string {
		mu.Lock()
		defer mu.Unlock()
		return lastLabelFilter
	}
}

func matchesAllLabels(labels map[string]string, filters []string) bool {
	for _, f := range filters {
		k, v, ok := strings.Cut(f, "=")
		if !ok || labels[k] != v {
			return false
		}
	}
	return true
}

func TestProjectDiscovery_Discover_UsesProjectLabel(t *testing.T) {
	projectContainer := fakeContainer{
		id:     "abc123",
		labels: map[string]string{"scion.name": "agent-a", "scion.project": "my-project", "scion.grove": "my-project"},
		mounts: []container.MountPoint{{Type: "bind", Source: "/host/agent-a", Destination: "/workspace"}},
	}
	groveOnlyContainer := fakeContainer{
		id:     "def456",
		labels: map[string]string{"scion.name": "agent-b", "scion.grove": "my-project"},
		mounts: []container.MountPoint{{Type: "bind", Source: "/host/agent-b", Destination: "/workspace"}},
	}

	cli, gotFilter := newFakeDockerClient(t, []fakeContainer{projectContainer, groveOnlyContainer})

	discovery := NewProjectDiscovery(cli, "my-project", false)
	dirs, err := discovery.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover failed: %v", err)
	}

	if filter := gotFilter(); filter != "scion.project=my-project" {
		t.Errorf("label filter = %q, want %q", filter, "scion.project=my-project")
	}

	if len(dirs) != 1 || dirs[0] != "/host/agent-a" {
		t.Errorf("dirs = %v, want [/host/agent-a] (the grove-only-labelled container must not be picked up)", dirs)
	}
}

func TestProjectDiscovery_DiscoverForContainer_UsesProjectLabel(t *testing.T) {
	projectContainer := fakeContainer{
		id:     "abc123",
		labels: map[string]string{"scion.project": "my-project"},
		mounts: []container.MountPoint{{Type: "bind", Source: "/host/agent-a", Destination: "/workspace"}},
	}
	groveOnlyContainer := fakeContainer{
		id:     "def456",
		labels: map[string]string{"scion.grove": "my-project"},
		mounts: []container.MountPoint{{Type: "bind", Source: "/host/agent-b", Destination: "/workspace"}},
	}

	cli, _ := newFakeDockerClient(t, []fakeContainer{projectContainer, groveOnlyContainer})
	discovery := NewProjectDiscovery(cli, "my-project", false)

	dir, err := discovery.DiscoverForContainer(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("DiscoverForContainer failed: %v", err)
	}
	if dir != "/host/agent-a" {
		t.Errorf("DiscoverForContainer(abc123) = %q, want /host/agent-a", dir)
	}

	dir, err = discovery.DiscoverForContainer(context.Background(), "def456")
	if err != nil {
		t.Fatalf("DiscoverForContainer failed: %v", err)
	}
	if dir != "" {
		t.Errorf("DiscoverForContainer(def456) = %q, want empty (grove-only label must not match)", dir)
	}
}
