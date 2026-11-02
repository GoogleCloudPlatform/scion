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

package hub

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
)

// setEnvForTest sets an env var and returns a cleanup function.
func setEnvForTest(t *testing.T, key, value string) {
	t.Helper()
	t.Setenv(key, value)
}

type fakeHubSettingStore struct {
	mu       sync.Mutex
	settings map[string]*store.HubSetting
	nextRev  map[string]int64
}

func newFakeHubSettingStore() *fakeHubSettingStore {
	return &fakeHubSettingStore{
		settings: make(map[string]*store.HubSetting),
		nextRev:  make(map[string]int64),
	}
}

func newFileKoanf(t *testing.T, flat map[string]interface{}) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		t.Fatalf("failed to build file koanf: %v", err)
	}
	return k
}

func newEnvKoanf(t *testing.T, flat map[string]interface{}) *koanf.Koanf {
	t.Helper()
	k := koanf.New(".")
	if err := k.Load(confmap.Provider(flat, "."), nil); err != nil {
		t.Fatalf("failed to build env koanf: %v", err)
	}
	return k
}

func emptyKoanf() *koanf.Koanf {
	return koanf.New(".")
}

func (f *fakeHubSettingStore) GetHubSetting(_ context.Context, section string) (*store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.settings[section]
	if !ok {
		return nil, store.ErrNotFound
	}
	return s, nil
}

func (f *fakeHubSettingStore) ListHubSettings(_ context.Context) ([]store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]store.HubSetting, 0, len(f.settings))
	for _, s := range f.settings {
		out = append(out, *s)
	}
	return out, nil
}

func (f *fakeHubSettingStore) UpsertHubSetting(_ context.Context, section string, value json.RawMessage, updatedBy string, expectedRevision int64, origin string) (*store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	existing, ok := f.settings[section]
	if ok {
		if expectedRevision > 0 && existing.Revision != expectedRevision {
			return nil, store.ErrRevisionConflict
		}
		if expectedRevision == 0 {
			return nil, store.ErrRevisionConflict
		}
	} else {
		if expectedRevision > 0 {
			return nil, store.ErrRevisionConflict
		}
	}

	rev := int64(1)
	if existing != nil {
		rev = existing.Revision + 1
	}
	s := &store.HubSetting{
		ID:        section,
		Section:   section,
		Value:     value,
		Revision:  rev,
		UpdatedBy: updatedBy,
		Origin:    origin,
	}
	f.settings[section] = s
	return s, nil
}

func (f *fakeHubSettingStore) DeleteHubSetting(_ context.Context, section string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.settings[section]; !ok {
		return store.ErrNotFound
	}
	delete(f.settings, section)
	return nil
}

func (f *fakeHubSettingStore) BackfillOrigin(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for section, s := range f.settings {
		if section == "_meta" {
			continue
		}
		if s.UpdatedBy != "seed" && s.Origin == "seeded" {
			s.Origin = "managed"
		}
	}
	return nil
}

// helper to seed a section directly.
func (f *fakeHubSettingStore) seed(section string, doc json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[section] = &store.HubSetting{
		ID:       section,
		Section:  section,
		Value:    doc,
		Revision: 1,
	}
}

func (f *fakeHubSettingStore) seedWithOrigin(section string, doc json.RawMessage, origin string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settings[section] = &store.HubSetting{
		ID:       section,
		Section:  section,
		Value:    doc,
		Revision: 1,
		Origin:   origin,
	}
}

// UpsertHubSettingsGuarded implements store.HubSettingGuardedBatchWriter
// for the fake: the read guards and expected revisions are checked first,
// then every write is applied, all under one lock.
func (f *fakeHubSettingStore) UpsertHubSettingsGuarded(_ context.Context, writes []store.HubSettingWrite, reads map[string]int64, updatedBy, origin string) ([]store.HubSetting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(writes) == 0 {
		return nil, store.ErrInvalidInput
	}
	current := func(section string) int64 {
		if s, ok := f.settings[section]; ok {
			return s.Revision
		}
		return 0
	}
	for section, rev := range reads {
		if current(section) != rev {
			return nil, store.ErrRevisionConflict
		}
	}
	for _, w := range writes {
		_, exists := f.settings[w.Section]
		switch {
		case w.Delete && !exists && w.ExpectedRevision == -1:
			return nil, store.ErrNotFound
		case w.Delete && !exists:
			return nil, store.ErrRevisionConflict
		case w.ExpectedRevision == -1:
		case w.ExpectedRevision == 0:
			if exists {
				return nil, store.ErrRevisionConflict
			}
		case !exists || current(w.Section) != w.ExpectedRevision:
			return nil, store.ErrRevisionConflict
		}
	}
	out := make([]store.HubSetting, 0, len(writes))
	for _, w := range writes {
		if w.Delete {
			delete(f.settings, w.Section)
			out = append(out, store.HubSetting{Section: w.Section})
			continue
		}
		s := &store.HubSetting{ID: w.Section, Section: w.Section, Value: w.Value, Revision: current(w.Section) + 1,
			UpdatedBy: updatedBy, Origin: origin}
		f.settings[w.Section] = s
		out = append(out, *s)
	}
	return out, nil
}
