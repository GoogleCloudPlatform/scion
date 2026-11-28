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
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
)

// TokenStoreMode selects where PID 1 stores its runtime agent token.
type TokenStoreMode int

const (
	// TokenStoreHome is the default non-isolated mode: the token lives in
	// <scion home>/.scion/scion-token, shared with workload processes.
	TokenStoreHome TokenStoreMode = iota
	// TokenStoreRuntimeFile stores the token at a root-only runtime path
	// (DefaultRuntimeTokenPath: /run/scion/scion-token, dir 0700, file 0600,
	// owned by root:root).
	TokenStoreRuntimeFile
	// TokenStoreMemory keeps the token in memory only and never persists it
	// to disk (used on rootless/shared-UID runtimes or when /run is not
	// writable).
	TokenStoreMemory
)

const (
	// DefaultRuntimeTokenDir is the root-only runtime directory for isolated
	// PID 1 credentials.
	DefaultRuntimeTokenDir = "/run/scion"
	// DefaultRuntimeTokenPath is the root-only runtime path for the isolated
	// agent token.
	DefaultRuntimeTokenPath = "/run/scion/" + TokenFile
	// DefaultRuntimeTransportTokenPath is the root-only runtime path for the
	// isolated transport token.
	DefaultRuntimeTransportTokenPath = "/run/scion/" + transportauth.TransportTokenFileName
)

// ErrHandoffUnlinkFailed is returned when PID 1 cannot remove the handoff
// token file (or verify that the handoff path is absent). Callers under shell
// workload isolation must fail closed and refuse to start the workload.
var ErrHandoffUnlinkFailed = errors.New("failed to unlink handoff token file")

var (
	tokenStoreMu           sync.RWMutex
	tokenStoreMode         TokenStoreMode = TokenStoreHome
	runtimeTokenPath       string
	memoryToken            string
	memoryTransportToken   string
	runtimeStoreOverridden bool
)

// CurrentTokenStoreMode returns the active runtime token store mode.
func CurrentTokenStoreMode() TokenStoreMode {
	tokenStoreMu.RLock()
	defer tokenStoreMu.RUnlock()
	return tokenStoreMode
}

// IsIsolatedTokenStore reports whether the package is configured in an
// isolated token store mode (TokenStoreRuntimeFile or TokenStoreMemory).
func IsIsolatedTokenStore() bool {
	return CurrentTokenStoreMode() != TokenStoreHome
}

// HandoffTokenFilePath always returns the broker handoff drop-box path
// (<scion home>/.scion/scion-token), regardless of the active TokenStoreMode.
func HandoffTokenFilePath() string {
	return filepath.Join(tokenHomeResolver(), ".scion", TokenFile)
}

// HandoffTokenTmpFilePath returns the stale temp-file path alongside the
// handoff token (<scion home>/.scion/scion-token.tmp).
func HandoffTokenTmpFilePath() string {
	return filepath.Join(tokenHomeResolver(), ".scion", TokenFile+".tmp")
}

// HandoffTransportTokenFilePath always returns the broker handoff drop-box
// path for the transport token (<scion home>/.scion/transport-token),
// regardless of the active TokenStoreMode.
func HandoffTransportTokenFilePath() string {
	return filepath.Join(tokenHomeResolver(), ".scion", transportauth.TransportTokenFileName)
}

// HandoffTransportTokenTmpFilePath returns the stale temp-file path alongside
// the handoff transport token (<scion home>/.scion/transport-token.tmp).
func HandoffTransportTokenTmpFilePath() string {
	return filepath.Join(tokenHomeResolver(), ".scion", transportauth.TransportTokenFileName+".tmp")
}

// SetRuntimeTokenStoreForTest permits tests to configure a runtime token store
// path and automatically restores the previous token-store state on cleanup.
func SetRuntimeTokenStoreForTest() func() {
	tokenStoreMu.Lock()
	origMode := tokenStoreMode
	origPath := runtimeTokenPath
	origMem := memoryToken
	origMemTransport := memoryTransportToken
	origOverridden := runtimeStoreOverridden
	runtimeStoreOverridden = true
	tokenStoreMu.Unlock()

	return func() {
		tokenStoreMu.Lock()
		tokenStoreMode = origMode
		runtimeTokenPath = origPath
		memoryToken = origMem
		memoryTransportToken = origMemTransport
		runtimeStoreOverridden = origOverridden
		tokenStoreMu.Unlock()
	}
}

// ResetTokenStore restores the default TokenStoreHome mode and clears any
// isolated runtime path or in-memory tokens.
func ResetTokenStore() {
	tokenStoreMu.Lock()
	tokenStoreMode = TokenStoreHome
	runtimeTokenPath = ""
	memoryToken = ""
	memoryTransportToken = ""
	tokenStoreMu.Unlock()
}

// isDefaultRuntimePath reports whether path targets DefaultRuntimeTokenDir.
func isDefaultRuntimePath(path string) bool {
	clean := filepath.Clean(path)
	return clean == DefaultRuntimeTokenDir || strings.HasPrefix(clean, DefaultRuntimeTokenDir+"/")
}

// checkRuntimeStoreTestGuard panics under `go test` if code attempts to touch
// the real /run/scion directory or any runtime token file without test
// isolation enabled.
func checkRuntimeStoreTestGuard(path, caller string) {
	if !testing.Testing() {
		return
	}
	tokenStoreMu.RLock()
	overridden := runtimeStoreOverridden
	tokenStoreMu.RUnlock()
	if isDefaultRuntimePath(path) && !overridden {
		panic(fmt.Sprintf("scion/hub: %s called during a test targeting %s without SetRuntimeTokenStoreForTest()", caller, path))
	}
	if !tokenHomeOverridden && !overridden {
		panic(fmt.Sprintf("scion/hub: %s called during a test without SetTokenHome() or SetRuntimeTokenStoreForTest()", caller))
	}
}

// ConfigureIsolatedTokenStore switches the package to the requested token
// store mode:
//   - TokenStoreHome: restores default <scion home>/.scion/scion-token storage.
//   - TokenStoreRuntimeFile: ensures the parent directory of runtimePath exists
//     with mode 0700 owned by this process (root:root when euid==0) via dirfd
//     no-follow, then redirects TokenFilePath/ReadTokenFile/WriteTokenFile to
//     runtimePath.
//   - TokenStoreMemory: keeps the token in memory only; TokenFilePath() returns
//     "" and WriteTokenFile never touches the filesystem.
//
// HandoffTokenFilePath() always continues to return <scion home>/.scion/scion-token.
func ConfigureIsolatedTokenStore(mode TokenStoreMode, runtimePath string) error {
	switch mode {
	case TokenStoreHome:
		ResetTokenStore()
		return nil
	case TokenStoreRuntimeFile:
		if runtimePath == "" || !filepath.IsAbs(runtimePath) {
			return fmt.Errorf("runtime token path must be an absolute path, got %q", runtimePath)
		}
		cleanPath := filepath.Clean(runtimePath)
		checkRuntimeStoreTestGuard(cleanPath, "ConfigureIsolatedTokenStore")
		if err := ensureRuntimeTokenDir(cleanPath); err != nil {
			return err
		}
		tokenStoreMu.Lock()
		tokenStoreMode = TokenStoreRuntimeFile
		runtimeTokenPath = cleanPath
		memoryToken = ""
		tokenStoreMu.Unlock()
		return nil
	case TokenStoreMemory:
		tokenStoreMu.Lock()
		tokenStoreMode = TokenStoreMemory
		runtimeTokenPath = ""
		tokenStoreMu.Unlock()
		return nil
	default:
		return fmt.Errorf("unknown token store mode: %d", mode)
	}
}

func snapshotTokenStore() (mode TokenStoreMode, rPath, memTok string) {
	tokenStoreMu.RLock()
	defer tokenStoreMu.RUnlock()
	return tokenStoreMode, runtimeTokenPath, memoryToken
}

func setMemoryToken(tok string) {
	tokenStoreMu.Lock()
	memoryToken = strings.TrimSpace(tok)
	tokenStoreMu.Unlock()
}

func getMemoryTransportToken() string {
	tokenStoreMu.RLock()
	defer tokenStoreMu.RUnlock()
	return memoryTransportToken
}

func setMemoryTransportToken(tok string) {
	tokenStoreMu.Lock()
	memoryTransportToken = strings.TrimSpace(tok)
	tokenStoreMu.Unlock()
}
