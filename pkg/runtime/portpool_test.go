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
	"strings"
	"sync"
	"testing"
)

// mustNewPortPool creates a PortPool or fails the test.
func mustNewPortPool(t *testing.T, min, max, perAgent int, hostURL string) *PortPool {
	t.Helper()
	pool, err := NewPortPool(min, max, perAgent, hostURL)
	if err != nil {
		t.Fatalf("NewPortPool(%d, %d, %d, %q) = %v", min, max, perAgent, hostURL, err)
	}
	return pool
}

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
			pool := mustNewPortPool(t, tt.min, tt.max, tt.perAgent, "")
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
	pool := mustNewPortPool(t, 8000, 8005, 2, "")

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
	pool := mustNewPortPool(t, 8000, 8003, 2, "")

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
	pool := mustNewPortPool(t, 8000, 8001, 2, "")
	// Should not panic
	pool.Release("does-not-exist")
}

func TestPortPoolAllocatedPorts(t *testing.T) {
	pool := mustNewPortPool(t, 8000, 8005, 2, "")

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
			name:    "trailing slash preserved (caller trims)",
			hostURL: "http://example.com/",
			want:    "http://example.com/",
		},
		{
			name:    "empty host URL",
			hostURL: "",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := mustNewPortPool(t, 8000, 9000, 2, tt.hostURL)
			if got := pool.HostURL(); got != tt.want {
				t.Errorf("HostURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPortPoolConcurrency(t *testing.T) {
	pool := mustNewPortPool(t, 8000, 8099, 2, "")

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

func TestNewPortPoolValidation(t *testing.T) {
	tests := []struct {
		name             string
		min, max         int
		perAgent         int
		wantErrSubstring string
	}{
		{"min > max", 9000, 8000, 2, "min (9000) > max (8000)"},
		{"min zero", 0, 8000, 2, "out of bounds"},
		{"max exceeds 65535", 1, 70000, 2, "out of bounds"},
		{"perAgent zero", 8000, 9000, 0, "perAgent must be positive"},
		{"perAgent negative", 8000, 9000, -1, "perAgent must be positive"},
		{"perAgent exceeds 26", 8000, 9000, 27, "must be ≤ 26"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, err := NewPortPool(tt.min, tt.max, tt.perAgent, "")
			if err == nil {
				t.Fatalf("expected error containing %q, got pool %+v", tt.wantErrSubstring, pool)
			}
			if !strings.Contains(err.Error(), tt.wantErrSubstring) {
				t.Errorf("error %q does not contain %q", err, tt.wantErrSubstring)
			}
		})
	}
}

func TestAllocateValidation(t *testing.T) {
	pool := mustNewPortPool(t, 8000, 9000, 2, "")

	// Empty agent name
	if _, err := pool.Allocate("", 2); err == nil {
		t.Fatal("expected error for empty agent name")
	}

	// Zero count
	if _, err := pool.Allocate("agent-a", 0); err == nil {
		t.Fatal("expected error for count=0")
	}

	// Negative count
	if _, err := pool.Allocate("agent-b", -1); err == nil {
		t.Fatal("expected error for count=-1")
	}
}

func TestAllocateDoubleAllocateSameAgent(t *testing.T) {
	pool := mustNewPortPool(t, 8000, 8005, 2, "")

	ports1, err := pool.Allocate("agent-1", 2)
	if err != nil {
		t.Fatalf("first allocate: %v", err)
	}
	if ports1[0] != 8000 || ports1[1] != 8001 {
		t.Errorf("first allocate got %v, want [8000 8001]", ports1)
	}

	// Second allocation for the same agent gets new ports
	ports2, err := pool.Allocate("agent-1", 2)
	if err != nil {
		t.Fatalf("second allocate: %v", err)
	}
	if ports2[0] != 8002 || ports2[1] != 8003 {
		t.Errorf("second allocate got %v, want [8002 8003]", ports2)
	}

	// All four ports are listed for agent-1
	all := pool.AllocatedPorts("agent-1")
	if len(all) != 4 {
		t.Fatalf("expected 4 ports for agent-1, got %d: %v", len(all), all)
	}
}

func TestAllocatedPortsAfterRelease(t *testing.T) {
	pool := mustNewPortPool(t, 8000, 8003, 2, "")
	pool.Allocate("agent-1", 2)

	pool.Release("agent-1")

	// After release, AllocatedPorts should return nil
	if ports := pool.AllocatedPorts("agent-1"); ports != nil {
		t.Errorf("expected nil after release, got %v", ports)
	}
}

func TestPortPoolTotalAndAvailable(t *testing.T) {
	pool := mustNewPortPool(t, 8000, 8009, 2, "")

	if pool.Total() != 10 {
		t.Errorf("Total() = %d, want 10", pool.Total())
	}
	if pool.Available() != 10 {
		t.Errorf("Available() = %d, want 10", pool.Available())
	}

	pool.Allocate("agent-1", 3)

	if pool.Available() != 7 {
		t.Errorf("Available() after allocating 3 = %d, want 7", pool.Available())
	}

	pool.Release("agent-1")

	if pool.Available() != 10 {
		t.Errorf("Available() after release = %d, want 10", pool.Available())
	}
}

func TestPortPoolConcurrentAllocateRelease(t *testing.T) {
	// Stress test: 20 goroutines each allocate 2 ports, release, and re-allocate.
	// The pool has 100 ports (8000-8099), so 50 concurrent 2-port allocations
	// are the max. By releasing between rounds, we exercise the concurrent
	// interleaving of allocate and release.
	pool := mustNewPortPool(t, 8000, 8099, 2, "")

	var wg sync.WaitGroup
	errs := make(chan error, 200)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("agent-%d", idx)
			for round := 0; round < 5; round++ {
				ports, err := pool.Allocate(name, 2)
				if err != nil {
					errs <- fmt.Errorf("agent-%d round %d allocate: %w", idx, round, err)
					return
				}
				if len(ports) != 2 {
					errs <- fmt.Errorf("agent-%d round %d: got %d ports", idx, round, len(ports))
					return
				}
				pool.Release(name)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}

	// After all releases, pool should be fully available
	if pool.Available() != 100 {
		t.Errorf("Available() = %d, want 100 after all releases", pool.Available())
	}
}
