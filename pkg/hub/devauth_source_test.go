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
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDevUserSourceSetIdentity(t *testing.T) {
	src := newDevUserSource(DevUserConfig{Username: "alice", DisplayName: "Alice", Email: "alice@example.com"})
	before := src.Load()

	src.SetIdentity("Alice Liddell", "")
	got := src.Load()
	require.Equal(t, "Alice Liddell", got.DisplayName())
	require.Equal(t, "alice@example.com", got.Email())
	require.Equal(t, "alice", got.Username())
	require.True(t, isTrustedLocalDevUser(got))

	// A value already handed to a request is never mutated in place.
	require.Equal(t, "Alice", before.DisplayName())

	src.SetIdentity("", "liddell@example.com")
	require.Equal(t, "Alice Liddell", src.Load().DisplayName())
	require.Equal(t, "liddell@example.com", src.Load().Email())
}

func TestDevUserSourceConcurrentUse(t *testing.T) {
	src := newDevUserSource(DevUserConfig{Username: "dev", DisplayName: "Dev", Email: "dev@example.com"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); src.SetIdentity("Writer", "writer@example.com") }()
		go func() { defer wg.Done(); _ = src.Load().DisplayName() }()
	}
	wg.Wait()
	require.Equal(t, "Writer", src.Load().DisplayName())
}

func TestAuthConfigCurrentDevUserFallsBackToConfig(t *testing.T) {
	cfg := AuthConfig{DevUserCfg: DevUserConfig{Username: "bob", DisplayName: "Bob", Email: "bob@example.com"}}
	require.Equal(t, "Bob", cfg.currentDevUser().DisplayName())

	cfg.DevUser = newDevUserSource(cfg.DevUserCfg)
	cfg.DevUser.SetIdentity("Robert", "")
	require.Equal(t, "Robert", cfg.currentDevUser().DisplayName())
}
