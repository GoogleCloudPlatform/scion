//go:build !unix

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

package termresize

import (
	"context"
	"time"

	"golang.org/x/term"
)

// pollInterval is how often the console size is checked: this platform
// has no SIGWINCH.
const pollInterval = 500 * time.Millisecond

// Notify delivers a value on the returned channel each time the console
// size of fd changes. stop ends the polling; it also ends when ctx does.
func Notify(ctx context.Context, fd int) (resized <-chan struct{}, stop func()) {
	out := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		lastW, lastH, _ := term.GetSize(fd)
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w, h, err := term.GetSize(fd)
				if err != nil || (w == lastW && h == lastH) {
					continue
				}
				lastW, lastH = w, h
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out, cancel
}
