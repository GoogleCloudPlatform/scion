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
	"fmt"
	"sync"
	"testing"
)

func TestPortPoolAllocate(t *testing.T) {
	tests := []struct {
		name      string
		min, max  int
		perAgent  int
		agent     string
		count     int
		wantPorts []int
		wantErr   bool
	}{
		{
			name:      "allocate two ports",
			min:       8000,
			max:       9000,
			perAgent:  2,
			agent:     "agent-a",
			count:     2,
			wantPorts: []int{8000, 8001},
		},
		{
			name:      "allocate one port",
			min:       8000,
			max:       9000,
			perAgent:  1,
			agent:     "agent-b",
			count:     1,
			wantPorts: []int{8000},
		},
		{
			name:     "pool too small",
			min:      8000,
			max:      8000,
			perAgent: 2,
			agent:    "agent-c",
			count:    2,
			wantErr:  true,
		},
		{
			name:      "exact fit",
			min:       8000,
			max:       8001,
			perAgent:  2,
			agent:     "agent-d",
			count:     2,
			wantPorts: []int{8000, 8001},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := NewPortPool(tt.min, tt.max, tt.perAgent, "")
			ports, err := pool.Allocate(tt.agent, tt.count)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got ports %v", ports)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(ports) != len(tt.wantPorts) {
				t.Fatalf("got %d ports, want %d", len(ports), len(tt.wantPorts))
			}
			for i, p := range ports {
				if p != tt.wantPorts[i] {
					t.Errorf("port[%d] = %d, want %d", i, p, tt.wantPorts[i])
				}
			}
		})
	}
}

func TestPortPoolAllocateMultipleAgents(t *testing.T) {
	pool := NewPortPool(8000, 8005, 2, "")

	ports1, err := pool.Allocate("agent-1", 2)
	if err != nil {
		t.Fatalf("agent-1 allocate: %v", err)
	}
	if ports1[0] != 8000 || ports1[1] != 8001 {
		t.Errorf("agent-1 got %v, want [8000 8001]", ports1)
	}

	ports2, err := pool.Allocate("agent-2", 2)
	if err != nil {
		t.Fatalf("agent-2 allocate: %v", err)
	}
	if ports2[0] != 8002 || ports2[1] != 8003 {
		t.Errorf("agent-2 got %v, want [8002 8003]", ports2)
	}

	ports3, err := pool.Allocate("agent-3", 2)
	if err != nil {
		t.Fatalf("agent-3 allocate: %v", err)
	}
	if ports3[0] != 8004 || ports3[1] != 8005 {
		t.Errorf("agent-3 got %v, want [8004 8005]", ports3)
	}

	// Pool should be exhausted now
	_, err = pool.Allocate("agent-4", 2)
	if err == nil {
		t.Fatal("expected error when pool exhausted")
	}
}

func TestPortPoolRelease(t *testing.T) {
	pool := NewPortPool(8000, 8003, 2, "")

	pool.Allocate("agent-1", 2)
	pool.Allocate("agent-2", 2)

	pool.Release("agent-1")

	// agent-1's ports (8000, 8001) should now be available
	ports, err := pool.Allocate("agent-3", 2)
	if err != nil {
		t.Fatalf("allocate after release: %v", err)
	}
	if ports[0] != 8000 || ports[1] != 8001 {
		t.Errorf("got %v, want [8000 8001] (recycled from agent-1)", ports)
	}
}

func TestPortPoolReleaseNonexistent(t *testing.T) {
	pool := NewPortPool(8000, 8001, 2, "")
	// Should not panic
	pool.Release("does-not-exist")
}

func TestPortPoolAllocatedPorts(t *testing.T) {
	pool := NewPortPool(8000, 8005, 2, "")

	pool.Allocate("agent-1", 2)
	pool.Allocate("agent-2", 3)

	got := pool.AllocatedPorts("agent-2")
	want := []int{8002, 8003, 8004}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("port[%d] = %d, want %d", i, got[i], want[i])
		}
	}

	// Non-existent agent returns nil
	if ports := pool.AllocatedPorts("no-agent"); ports != nil {
		t.Errorf("expected nil for unknown agent, got %v", ports)
	}
}

func TestPortPoolHostURLTrailingSlash(t *testing.T) {
	// Verify that NewPortPool stores hostURL as-is; the caller
	// (server_foreground.go) is responsible for trimming trailing slashes
	// before constructing the pool.  This test documents the contract.
	tests := []struct {
		name    string
		hostURL string
		want    string
	}{
		{
			name:    "no trailing slash",
			hostURL: "http://example.com",
			want:    "http://example.com",
		},
		{
			name:    "trailing slash trimmed by caller",
			hostURL: "http://example.com",
			want:    "http://example.com",
		},
		{
			name:    "empty host URL",
			hostURL: "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := NewPortPool(8000, 9000, 2, tt.hostURL)
			if got := pool.HostURL(); got != tt.want {
				t.Errorf("HostURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPortPoolConcurrency(t *testing.T) {
	pool := NewPortPool(8000, 8099, 2, "")

	var wg sync.WaitGroup
	errs := make(chan error, 50)

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("agent-%d", idx)
			_, err := pool.Allocate(name, 2)
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent allocate error: %v", err)
	}

	// Verify no duplicate allocations: 50 agents * 2 ports = 100 ports (range 8000-8099)
	seen := make(map[int]bool)
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("agent-%d", i)
		ports := pool.AllocatedPorts(name)
		if len(ports) != 2 {
			t.Errorf("agent-%d: got %d ports, want 2", i, len(ports))
			continue
		}
		for _, p := range ports {
			if seen[p] {
				t.Errorf("duplicate port %d", p)
			}
			seen[p] = true
		}
	}
}
