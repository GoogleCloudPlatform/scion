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

//go:build !no_sqlite

package hub

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
)

// TestNew_StoresServerConfigExperimentsRegistry exercises the New()
// assignment that carries ServerConfig.Experiments into s.experiments, the
// only path a real server has for injecting a non-default registry. Every
// other test in this package builds &Server{experiments: ...} by struct
// literal, which never runs this line.
func TestNew_StoresServerConfigExperimentsRegistry(t *testing.T) {
	st, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() }) // Release in-memory SQLite database to avoid OOM across many tests.

	reg := testRegistry(t)
	cfg := DefaultServerConfig()
	cfg.Experiments = reg
	srv, err := New(cfg, st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	if srv.experimentRegistry() != reg {
		t.Fatal("New() did not carry cfg.Experiments into s.experiments; experimentRegistry() returned a different registry")
	}
	if !srv.experimentEnabled("hub.test_gate") {
		t.Error("hub.test_gate should resolve to its default (true) through the server built by New()")
	}

	// A server built without cfg.Experiments falls back to experiments.Default().
	srv2, err := New(DefaultServerConfig(), st)
	if err != nil {
		t.Fatalf("New (no Experiments): %v", err)
	}
	t.Cleanup(func() { _ = srv2.Shutdown(context.Background()) })
	if srv2.experimentRegistry() != experiments.Default() {
		t.Error("New() without cfg.Experiments should fall back to experiments.Default()")
	}
}
