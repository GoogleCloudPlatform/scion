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
	"errors"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// A run-scoped stop resolves its target exactly as a run-scoped delete does
// (ptone/scion#2550 P5, review N2 of ptone/scion#3076): for the same
// project, name, run and listed entries, lookupAgentMatchForRun and
// resolveDeleteTarget pick the same entry, or give the same answer
// (another run holds the name, not found, ambiguous, list unavailable).
func TestStopDeleteResolutionParity(t *testing.T) {
	type outcome = string
	const (
		notFound    = "not found"
		ambiguous   = "ambiguous"
		unavailable = "unavailable"
		files       = "files"
	)
	mismatch := func(current string) outcome { return "mismatch:" + current }
	match := func(cid string) outcome { return "match:" + cid }

	stopOutcome := func(m agentMatch, err error) outcome {
		var other *otherRunsHoldNameError
		switch {
		case errors.As(err, &other):
			return mismatch(other.currentRunID)
		case err != nil && strings.Contains(err.Error(), "ambiguous"):
			return ambiguous
		case errors.Is(err, ErrAgentListUnavailable):
			return unavailable
		case errors.Is(err, ErrAgentNotFound) && m.matched:
			return files
		case errors.Is(err, ErrAgentNotFound):
			return notFound
		case err != nil:
			return "error: " + err.Error()
		}
		return match(m.containerID)
	}
	deleteOutcome := func(tg *deleteTarget, err error) outcome {
		var rm *deleteRunMismatchError
		switch {
		case errors.As(err, &rm):
			return mismatch(rm.current)
		case err != nil && strings.Contains(err.Error(), "ambiguous"):
			return ambiguous
		case errors.Is(err, errDeleteTargetUnknown):
			return unavailable
		case errors.Is(err, errDeleteTargetNotFound):
			return notFound
		case err != nil:
			return "error: " + err.Error()
		case tg.containerID == "":
			return files
		}
		return match(tg.containerID)
	}

	cases := []struct {
		name    string
		agents  func(scionB string) []api.AgentInfo
		listErr bool
		// auxFails registers an auxiliary runtime whose List fails, beside
		// the default runtime listing agents.
		auxFails bool
		run      string
		want     outcome
	}{
		{
			name: "requested run's entry",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-a", scopeProjB, b), "run-a")}
			},
			run: "run-a", want: match("cid-a"),
		},
		{
			name: "only another run holds the name",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-b", scopeProjB, b), "run-b")}
			},
			run: "run-a", want: mismatch("run-b"),
		},
		{
			name: "requested run beside another run",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-b", scopeProjB, b), "run-b"), withRun(labelled("dev", "cid-a", scopeProjB, b), "run-a")}
			},
			run: "run-a", want: match("cid-a"),
		},
		{
			name:   "legacy unlabelled container matches by name",
			agents: func(b string) []api.AgentInfo { return []api.AgentInfo{labelled("dev", "cid-legacy", scopeProjB, b)} },
			run:    "run-a", want: match("cid-legacy"),
		},
		{
			name: "legacy unlabelled container beside another run",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-b", scopeProjB, b), "run-b"), labelled("dev", "cid-legacy", scopeProjB, b)}
			},
			run: "run-a", want: match("cid-legacy"),
		},
		{
			name: "two other runs",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-b", scopeProjB, b), "run-b"), withRun(labelled("dev", "cid-c", scopeProjB, b), "run-c")}
			},
			run: "run-a", want: mismatch(""),
		},
		{
			// Before P5 a run-scoped stop matched the file-only entry here
			// and answered 202 not found; the files are the other run's.
			name: "another run beside a file-only entry",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-b", scopeProjB, b), "run-b"), labelled("dev", "", scopeProjB, b)}
			},
			run: "run-a", want: mismatch("run-b"),
		},
		{
			name:   "file-only entry alone",
			agents: func(b string) []api.AgentInfo { return []api.AgentInfo{labelled("dev", "", scopeProjB, b)} },
			run:    "run-a", want: files,
		},
		{
			name: "two entries of the requested run",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-a1", scopeProjB, b), "run-a"), withRun(labelled("dev", "cid-a2", scopeProjB, b), "run-a")}
			},
			run: "run-a", want: ambiguous,
		},
		{
			name: "same name in another project only",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-x", scopeProjA, ""), "run-a")}
			},
			run: "run-a", want: notFound,
		},
		{
			name: "pre-label container whose path is the project's",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{{Name: "dev", ContainerID: "cid-pre", ProjectPath: b,
					Labels: map[string]string{"scion.agent": "true", "scion.name": "dev"}}}
			},
			run: "run-a", want: match("cid-pre"),
		},
		{
			// Before P5 a run-scoped stop accepted any container with no
			// project label; delete requires the path to identify as the
			// project.
			name: "pre-label container with an unidentified path",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{{Name: "dev", ContainerID: "cid-pre", ProjectPath: "/elsewhere/.scion",
					Labels: map[string]string{"scion.agent": "true", "scion.name": "dev"}}}
			},
			run: "run-a", want: notFound,
		},
		{
			name: "one other run in two namespaces",
			agents: func(b string) []api.AgentInfo {
				pod := func(ns, cid, run string, b string) api.AgentInfo {
					e := withRun(labelled("dev", cid, scopeProjB, b), run)
					e.Kubernetes = &api.AgentK8sMetadata{Namespace: ns, PodName: "dev"}
					return e
				}
				return []api.AgentInfo{pod("ns-1", "dev", "run-b", b), pod("ns-2", "dev", "run-b", b)}
			},
			run: "run-a", want: mismatch("run-b"),
		},
		{
			name: "two legacy entries",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{labelled("dev", "cid-l1", scopeProjB, b), labelled("dev", "cid-l2", scopeProjB, b)}
			},
			run: "run-a", want: ambiguous,
		},
		{
			// Another run holds the name in the runtime that listed, and a
			// second runtime could not be listed: the requested run may be
			// there, so neither answers run mismatch.
			name: "another run beside a runtime that cannot be listed",
			agents: func(b string) []api.AgentInfo {
				return []api.AgentInfo{withRun(labelled("dev", "cid-b", scopeProjB, b), "run-b")}
			},
			auxFails: true,
			run:      "run-a", want: unavailable,
		},
		{
			name:    "runtime list fails",
			agents:  func(b string) []api.AgentInfo { return nil },
			listErr: true,
			run:     "run-a", want: unavailable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &filteringMockManager{}
			srv, home := newScopeTestServer(t, mgr)
			// The project directory holds no files for "dev", so a delete
			// finds no file-only target beyond what the runtime lists.
			scionB, _ := makeHubProject(t, home, "proj-b", scopeProjB, "someone-else")
			mgr.agents = tc.agents(scionB)
			if tc.listErr {
				mgr.listErr = errors.New("list failed")
			}
			if tc.auxFails {
				aux := &filteringMockManager{}
				aux.listErr = errors.New("aux list failed")
				srv.auxiliaryRuntimesMu.Lock()
				srv.auxiliaryRuntimes["k8s-aux"] = auxiliaryRuntime{
					Runtime: &runtime.MockRuntime{NameFunc: func() string { return "kubernetes" }},
					Manager: aux,
				}
				srv.auxiliaryRuntimesMu.Unlock()
			}
			ctx := context.Background()

			gotStop := stopOutcome(srv.lookupAgentMatchForRun(ctx, "dev", scopeProjB, tc.run))
			gotDelete := deleteOutcome(srv.resolveDeleteTarget(ctx, "dev", scopeProjB, tc.run, "", false))
			if gotStop != tc.want {
				t.Errorf("stop: %s, want %s", gotStop, tc.want)
			}
			if gotDelete != tc.want {
				t.Errorf("delete: %s, want %s", gotDelete, tc.want)
			}
		})
	}
}
