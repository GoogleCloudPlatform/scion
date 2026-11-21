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

package cmd

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerconvert"
)

// acquireConvertStartLock is the server start check against
// 'runtime-broker convert' (brokerconvert.AcquireServerStartLock): a
// refusing try-lock, never a blocking wait. On a clean HOME with no
// ~/.scion yet, it creates globalDir with the same permissions as
// `scion init --machine` (0o755) before opening the owner-only convert
// tree beneath it. A variable so tests can observe the call.
var acquireConvertStartLock = acquireServerConvertStartLock

func acquireServerConvertStartLock(globalDir string) (*brokerconvert.HostLock, error) {
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create global directory: %w", err)
	}
	return brokerconvert.AcquireServerStartLock(globalDir)
}

// brokerListeners collects the Listening channels of the broker listeners
// this process starts (the legacy Runtime Broker server, or the flat
// instance host). The convert host lock taken at server start is released
// only once every one of them is closed: the release follows this
// process's own bind, never a network probe another process could answer.
// It is a process global that is only appended to, never reset: that
// holds because a process runs at most one server start.
var brokerListeners listenerSet

type listenerSet struct {
	mu  sync.Mutex
	chs []<-chan struct{}
}

// add records the Listening channel of one broker listener.
func (l *listenerSet) add(ch <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.chs = append(l.chs, ch)
}

func (l *listenerSet) snapshot() []<-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]<-chan struct{}(nil), l.chs...)
}

// releaseConvertLockWhenListening releases lock once every listener in
// chs has signalled that it is bound. When ctx ends first (the server is
// stopping, or a listener failed to bind and the start is failing) it
// returns without releasing: the caller's deferred Release frees the lock.
// With no listener there is nothing to wait for, and the lock is released.
func releaseConvertLockWhenListening(ctx context.Context, lock *brokerconvert.HostLock, chs []<-chan struct{}) {
	for _, ch := range chs {
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
	lock.Release()
}
