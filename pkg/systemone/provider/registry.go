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

package provider

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

var (
	// ErrNilAdapter indicates a nil Adapter was passed to Register.
	ErrNilAdapter = errors.New("provider: adapter must not be nil")
	// ErrInvalidAdapterMetadata indicates an Adapter reported an empty Name or Version.
	ErrInvalidAdapterMetadata = errors.New("provider: adapter name and version must be non-empty")
	// ErrNotSideEffectFree indicates an Adapter failed the SideEffectFree registration gate.
	ErrNotSideEffectFree = errors.New("provider: adapter must declare SideEffectFree: true to register")
	// ErrDuplicateAdapter indicates an Adapter with the same Name is already registered.
	ErrDuplicateAdapter = errors.New("provider: duplicate adapter registration")
)

// Registry is a concurrency-safe catalog of registered system-one Adapters.
type Registry struct {
	mu       sync.RWMutex
	adapters map[string]Adapter
}

// NewRegistry creates an empty Adapter Registry.
func NewRegistry() *Registry {
	return &Registry{
		adapters: make(map[string]Adapter),
	}
}

// Register validates and registers a into r. Per runtime-design §4.3, adapters
// must report a non-empty Name and Version and declare SideEffectFree: true.
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return ErrNilAdapter
	}
	name := a.Name()
	version := a.Version()
	if name == "" || version == "" {
		return fmt.Errorf("%w: name=%q version=%q", ErrInvalidAdapterMetadata, name, version)
	}
	caps, err := a.Capabilities(InstanceView{})
	if err != nil {
		return fmt.Errorf("provider: inspecting capabilities for %q: %w", name, err)
	}
	if !caps.SideEffectFree {
		return fmt.Errorf("%w: %q", ErrNotSideEffectFree, name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.adapters[name]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateAdapter, name)
	}
	r.adapters[name] = a
	return nil
}

// MustRegister registers a in r and panics on error.
func (r *Registry) MustRegister(a Adapter) {
	if err := r.Register(a); err != nil {
		panic(err)
	}
}

// Get looks up a registered Adapter by name.
func (r *Registry) Get(name string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.adapters[name]
	return a, ok
}

// Lookup is an alias for Get.
func (r *Registry) Lookup(name string) (Adapter, bool) {
	return r.Get(name)
}

// Names returns all registered adapter names in sorted lexicographical order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.adapters))
	for name := range r.adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var defaultRegistry = NewRegistry()

// DefaultRegistry returns the process-wide Adapter Registry into which compiled
// adapter packages register their profiles.
func DefaultRegistry() *Registry {
	return defaultRegistry
}

// Register adds a to DefaultRegistry.
func Register(a Adapter) error {
	return defaultRegistry.Register(a)
}

// MustRegister adds a to DefaultRegistry and panics on error.
func MustRegister(a Adapter) {
	defaultRegistry.MustRegister(a)
}

// Lookup looks up name in DefaultRegistry.
func Lookup(name string) (Adapter, bool) {
	return defaultRegistry.Get(name)
}

// Names returns all adapter names registered in DefaultRegistry in sorted order.
func Names() []string {
	return defaultRegistry.Names()
}
