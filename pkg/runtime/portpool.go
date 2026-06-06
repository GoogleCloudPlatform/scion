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
)

type PortPool struct {
	mu        sync.Mutex
	min       int
	max       int
	perAgent  int
	hostURL   string
	allocated map[int]string // port -> agentName
}

func NewPortPool(min, max, perAgent int, hostURL string) (*PortPool, error) {
	if min < 1 || max > 65535 {
		return nil, fmt.Errorf("port range [%d, %d] out of bounds (must be 1–65535)", min, max)
	}
	if min > max {
		return nil, fmt.Errorf("port range min (%d) > max (%d)", min, max)
	}
	if perAgent <= 0 {
		return nil, fmt.Errorf("perAgent must be positive, got %d", perAgent)
	}
	if perAgent > 26 {
		return nil, fmt.Errorf("perAgent must be ≤ 26 (env var suffixes use A–Z), got %d", perAgent)
	}
	return &PortPool{
		min:       min,
		max:       max,
		perAgent:  perAgent,
		hostURL:   hostURL,
		allocated: make(map[int]string),
	}, nil
}

// HostURL returns the base URL for constructing agent port URLs.
func (p *PortPool) HostURL() string {
	return p.hostURL
}

// PerAgent returns the default number of ports allocated per agent.
func (p *PortPool) PerAgent() int {
	return p.perAgent
}

// Allocate picks the lowest available ports for the given agent.
func (p *PortPool) Allocate(agentName string, count int) ([]int, error) {
	if agentName == "" {
		return nil, fmt.Errorf("agent name must not be empty")
	}
	if count <= 0 {
		return nil, fmt.Errorf("count must be positive, got %d", count)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	var ports []int
	for port := p.min; port <= p.max && len(ports) < count; port++ {
		if _, taken := p.allocated[port]; !taken {
			ports = append(ports, port)
		}
	}
	if len(ports) < count {
		return nil, fmt.Errorf("port pool exhausted: need %d ports, only %d available", count, len(ports))
	}
	for _, port := range ports {
		p.allocated[port] = agentName
	}
	return ports, nil
}

// Release frees all ports allocated to the given agent.
func (p *PortPool) Release(agentName string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for port, name := range p.allocated {
		if name == agentName {
			delete(p.allocated, port)
		}
	}
}

// AllocatedPorts returns the ports currently allocated to the given agent.
// The returned slice is sorted in ascending order.
func (p *PortPool) AllocatedPorts(agentName string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()

	var ports []int
	for port := p.min; port <= p.max; port++ {
		if p.allocated[port] == agentName {
			ports = append(ports, port)
		}
	}
	return ports
}

// Total returns the total number of ports in the pool (allocated + free).
func (p *PortPool) Total() int {
	return p.max - p.min + 1
}

// Available returns the number of unallocated ports.
func (p *PortPool) Available() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.Total() - len(p.allocated)
}
