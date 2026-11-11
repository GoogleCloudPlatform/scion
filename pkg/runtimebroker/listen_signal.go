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

package runtimebroker

import "sync"

// listenSignal is a channel closed once, when a listener is bound. The
// zero value is ready to use.
type listenSignal struct {
	init  sync.Once
	close sync.Once
	c     chan struct{}
}

func (l *listenSignal) ch() <-chan struct{} {
	l.init.Do(func() { l.c = make(chan struct{}) })
	return l.c
}

// mark closes the channel; later calls do nothing.
func (l *listenSignal) mark() {
	l.ch()
	l.close.Do(func() { close(l.c) })
}
