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

package integration_test

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type loadAlternator struct {
	backends [2]*httputil.ReverseProxy
	next     atomic.Uint64
}

func newLoadAlternator(rawBackend1, rawBackend2 string) (*loadAlternator, error) {
	var alternator loadAlternator
	for index, rawURL := range []string{rawBackend1, rawBackend2} {
		backend, err := url.Parse(rawURL)
		if err != nil {
			return nil, fmt.Errorf("parse backend %d: %w", index+1, err)
		}
		if backend.Scheme != "http" || backend.Host == "" {
			return nil, fmt.Errorf("backend %d must be an absolute HTTP URL", index+1)
		}
		proxy := httputil.NewSingleHostReverseProxy(backend)
		proxy.FlushInterval = -1
		alternator.backends[index] = proxy
	}
	return &alternator, nil
}

func (a *loadAlternator) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	index := (a.next.Add(1) - 1) % uint64(len(a.backends))
	a.backends[index].ServeHTTP(response, request)
}

func TestHarnessHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	address := os.Getenv(helperAddressEnv)
	switch mode {
	case "backend":
		serveBackendProcess(t, address, os.Getenv(helperReplicaIDEnv))
	case "backend-log":
		credential := os.Getenv("SCION_TEST_SYNTHETIC_CREDENTIAL")
		digest := sha256.Sum256([]byte(credential))
		fmt.Printf("Authorization: Bearer %s token_hash=%s\n", credential, hex.EncodeToString(digest[:]))
		serveBackendProcess(t, address, os.Getenv(helperReplicaIDEnv))
	case "alternator":
		alternator, err := newLoadAlternator(os.Getenv("SCION_TEST_BACKEND_1"), os.Getenv("SCION_TEST_BACKEND_2"))
		if err != nil {
			t.Fatal(err)
		}
		serveHTTPProcess(t, address, alternator)
	case "fake-google":
		serveHTTPProcess(t, address, newFakeGoogleProcess(t))
	case "hub":
		serveHubProcess(t, address)
	case "auth-bridge":
		serveAuthBridgeProcess(t, address, os.Getenv(helperReplicaIDEnv))
	case "full-bridge":
		serveFullBridgeProcess(t, address, os.Getenv(helperReplicaIDEnv))
	case "ha-bridge":
		serveHABridgeProcess(t, address, os.Getenv(helperReplicaIDEnv))
	case "grpc-control":
		serveControlGRPCProcess(t, address)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func serveBackendProcess(t *testing.T, address, replicaID string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/request", func(response http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(response, replicaID)
	})
	mux.HandleFunc("/stream", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := response.(http.Flusher)
		if !ok {
			t.Error("response writer does not support flushing")
			return
		}
		for range 3 {
			fmt.Fprintf(response, "data: %s\n\n", replicaID)
			flusher.Flush()
			time.Sleep(15 * time.Millisecond)
		}
	})
	serveHTTPProcess(t, address, mux)
}

func serveHTTPProcess(t *testing.T, address string, handler http.Handler) {
	t.Helper()
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: time.Second}
	listener := inheritedHelperListener(t, address)
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		t.Fatal(err)
	}
}

func inheritedHelperListener(t *testing.T, address string) net.Listener {
	t.Helper()
	fd, err := strconv.Atoi(os.Getenv(helperListenerFDEnv))
	if err != nil || fd < 3 {
		t.Fatalf("invalid inherited listener fd %q", os.Getenv(helperListenerFDEnv))
	}
	file := os.NewFile(uintptr(fd), "integration-helper-listener")
	if file == nil {
		t.Fatalf("open inherited listener fd %d", fd)
	}
	listener, err := net.FileListener(file)
	_ = file.Close()
	if err != nil {
		t.Fatalf("open inherited listener for %s: %v", address, err)
	}
	if listener.Addr().String() != address {
		_ = listener.Close()
		t.Fatalf("inherited listener address = %s, want %s", listener.Addr(), address)
	}
	return listener
}
