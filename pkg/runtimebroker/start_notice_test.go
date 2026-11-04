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

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// noticeManager is asyncManager whose Start reports a start notice.
type noticeManager struct {
	*asyncManager
	notice string
}

func (m *noticeManager) Start(ctx context.Context, opts api.StartOptions) (*api.AgentInfo, error) {
	info, err := m.asyncManager.Start(ctx, opts)
	if info != nil {
		info.StartNotice = m.notice
	}
	return info, err
}

// succeededReportFor runs one async launch with mgr and returns its succeeded report.
func succeededReportFor(t *testing.T, mgr *noticeManager, slug string) *hubclient.AgentLaunchReport {
	t.Helper()
	srv, rtb := newAsyncTestServer(t, mgr.asyncManager)
	var mu sync.Mutex
	var terminal *hubclient.AgentLaunchReport
	rtb.launchReportFunc = func(req *hubclient.AgentLaunchReport) (*hubclient.AgentLaunchReportResult, error) {
		if req.State == hubclient.AgentLaunchReportStateSucceeded {
			mu.Lock()
			terminal = req
			mu.Unlock()
		}
		return &hubclient.AgentLaunchReportResult{Result: hubclient.AgentLaunchReportResultApplied}, nil
	}
	rec := newLaunchRecord("L-"+slug, slug, store.LaunchKindStart, "", time.Now().Add(5*time.Minute), func() {})
	lc := launchCtx{opts: api.StartOptions{Name: slug}, mgr: mgr, key: launchKey{Slug: slug}}
	done := make(chan struct{})
	go func() {
		srv.runLaunch(context.Background(), rec, lc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runLaunch did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	if terminal == nil {
		t.Fatal("no succeeded report was sent")
	}
	return terminal
}

// The succeeded report carries the start's notice (for example the repository
// git config keys left out of the agent's private copy) as its message, so the
// hub shows it as the agent's status message. No notice means no message.
func TestRunLaunch_SucceededReportCarriesStartNotice(t *testing.T) {
	const notice = "agent git config: left out core.editor from the agent's copy of .git/config; settings that name a command or a hook location are never given to agents"
	if got := succeededReportFor(t, &noticeManager{asyncManager: newAsyncManager(), notice: notice}, "agent-notice").Message; got != notice {
		t.Errorf("succeeded report message = %q, want the start notice", got)
	}
	if got := succeededReportFor(t, &noticeManager{asyncManager: newAsyncManager()}, "agent-nonotice").Message; got != "" {
		t.Errorf("succeeded report message without a notice = %q, want empty", got)
	}
}
