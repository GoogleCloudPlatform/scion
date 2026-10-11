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
	"sync"
)

// fenceRecordingClient records each delete's options and refuses the
// delete of staleRun with 409 stale_dispatch.
type fenceRecordingClient struct {
	*mockRuntimeBrokerClient
	staleRun string

	mu      sync.Mutex
	deletes []DeleteAgentOptions
}

func runsOf(opts []DeleteAgentOptions) []string {
	runs := make([]string, 0, len(opts))
	for _, o := range opts {
		runs = append(runs, o.RunID)
	}
	return runs
}

func (c *fenceRecordingClient) DeleteAgent(_ context.Context, _, _, _, _ string, opts DeleteAgentOptions) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, opts)
	if c.staleRun != "" && opts.RunID == c.staleRun {
		return staleDispatchErr()
	}
	return nil
}

func (c *fenceRecordingClient) sent() []DeleteAgentOptions {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]DeleteAgentOptions(nil), c.deletes...)
}
