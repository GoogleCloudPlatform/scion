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

package conduittest

import (
	"context"
	"io"
	"sync"

	sconduit "github.com/GoogleCloudPlatform/scion/pkg/sciontool/conduit"
)

// FakePTY implements sconduit.PTYProcess by echoing written input back to Read
// and recording resize events and closure.
type FakePTY struct {
	Req     sconduit.PTYRequest
	pr      *io.PipeReader
	pw      *io.PipeWriter
	Resizes chan [2]uint16
	Closed  chan struct{}
	once    sync.Once
}

// NewFakePTY constructs an echoing FakePTY for req.
func NewFakePTY(req sconduit.PTYRequest) *FakePTY {
	pr, pw := io.Pipe()
	return &FakePTY{
		Req:     req,
		pr:      pr,
		pw:      pw,
		Resizes: make(chan [2]uint16, 16),
		Closed:  make(chan struct{}),
	}
}

// Read implements io.Reader.
func (p *FakePTY) Read(b []byte) (int, error) { return p.pr.Read(b) }

// Write implements io.Writer.
func (p *FakePTY) Write(b []byte) (int, error) { return p.pw.Write(b) }

// Resize implements sconduit.PTYProcess.
func (p *FakePTY) Resize(cols, rows uint16) error {
	select {
	case p.Resizes <- [2]uint16{cols, rows}:
	default:
	}
	return nil
}

// Close implements io.Closer. Safe to call multiple times concurrently.
func (p *FakePTY) Close() error {
	p.once.Do(func() {
		close(p.Closed)
		_ = p.pw.Close()
		_ = p.pr.Close()
	})
	return nil
}

// FakePTYSpawner spawns FakePTY instances and publishes each one on Spawned.
type FakePTYSpawner struct {
	Spawned chan *FakePTY
}

// NewFakePTYSpawner creates a FakePTYSpawner with a buffered Spawned channel.
func NewFakePTYSpawner() *FakePTYSpawner {
	return &FakePTYSpawner{
		Spawned: make(chan *FakePTY, 32),
	}
}

// Spawn implements sconduit.PTYSpawner.
func (s *FakePTYSpawner) Spawn(_ context.Context, req sconduit.PTYRequest) (sconduit.PTYProcess, error) {
	p := NewFakePTY(req)
	select {
	case s.Spawned <- p:
	default:
	}
	return p, nil
}
