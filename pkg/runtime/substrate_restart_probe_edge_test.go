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

package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

const probeEdgeProjectID = "550e8400-e29b-41d4-a716-446655440000"
const probeEdgeAtespace = "scion-550e8400-e29"

func probeEdgeActor(name, uid string, status *ateapipb.ActorStatus) *ateapipb.Actor {
	return &ateapipb.Actor{
		Metadata: &ateapipb.ResourceMetadata{Atespace: probeEdgeAtespace, Name: name, Uid: uid},
		Status:   status,
	}
}

// A record-less actor listed only on a later page must still be counted:
// the probe has to walk every page, not just the first.
func TestSubstrateRestart_RecordlessActors_CountsActorsOnLaterPages(t *testing.T) {
	rt, fc, _, closeServer := newTestSubstrateHarness(t, &callRecorder{})
	defer closeServer()

	running := &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING}
	fc.listActors = func(req *ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		switch req.GetPageToken() {
		case "":
			return &ateapipb.ListActorsResponse{NextPageToken: "p2"}, nil
		case "p2":
			return &ateapipb.ListActorsResponse{
				Actors: []*ateapipb.Actor{probeEdgeActor("late-agent", "uid-late", running)},
			}, nil
		default:
			return nil, fmt.Errorf("unexpected page token %q", req.GetPageToken())
		}
	}

	_, actors, err := rt.RecordlessActors(context.Background(), probeEdgeProjectID)
	names := recordlessNames(actors)
	if err != nil {
		t.Fatalf("RecordlessActors() error = %v", err)
	}
	if len(names) != 1 || names[0] != "late-agent" {
		t.Errorf("RecordlessActors() = %v, want [late-agent] from page 2", names)
	}
}

// A server that echoes the same next_page_token forever must produce an
// explicit error, never a hang and never a "none found" result.
func TestSubstrateRestart_RecordlessActors_RepeatedPageTokenIsExplicitError(t *testing.T) {
	rt, fc, _, closeServer := newTestSubstrateHarness(t, &callRecorder{})
	defer closeServer()

	calls := 0
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		calls++
		if calls > 3 {
			return nil, fmt.Errorf("probe kept paging after a repeated token (call %d)", calls)
		}
		return &ateapipb.ListActorsResponse{NextPageToken: "same"}, nil
	}

	_, actors, err := rt.RecordlessActors(context.Background(), probeEdgeProjectID)
	names := recordlessNames(actors)
	if err == nil || !strings.Contains(err.Error(), "repeated page token") {
		t.Fatalf("RecordlessActors() err = %v, want a repeated-page-token error", err)
	}
	if names != nil {
		t.Errorf("RecordlessActors() names = %v, want nil alongside the error", names)
	}
	if calls != 2 {
		t.Errorf("ListActors calls = %d, want 2 (stop at the first repeated token)", calls)
	}
}

// A server that returns a fresh next_page_token on every call must be cut
// off by the page cap with an explicit error.
func TestSubstrateRestart_RecordlessActors_EndlessPagingHitsPageCap(t *testing.T) {
	rt, fc, _, closeServer := newTestSubstrateHarness(t, &callRecorder{})
	defer closeServer()

	// maxRecordlessActorListPages is a package var (not a const) precisely
	// so this test can lower it, rather than paging through the real
	// production cap's worth of fake responses.
	origCap := maxRecordlessActorListPages
	maxRecordlessActorListPages = 5
	t.Cleanup(func() { maxRecordlessActorListPages = origCap })

	calls := 0
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		calls++
		if calls > maxRecordlessActorListPages+1 {
			return nil, fmt.Errorf("probe exceeded the page cap (call %d)", calls)
		}
		return &ateapipb.ListActorsResponse{NextPageToken: fmt.Sprintf("p%d", calls)}, nil
	}

	_, actors, err := rt.RecordlessActors(context.Background(), probeEdgeProjectID)
	names := recordlessNames(actors)
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("RecordlessActors() err = %v, want a page-cap error", err)
	}
	if names != nil {
		t.Errorf("RecordlessActors() names = %v, want nil alongside the error", names)
	}
	if calls != maxRecordlessActorListPages {
		t.Errorf("ListActors calls = %d, want exactly %d", calls, maxRecordlessActorListPages)
	}
}

// Only ACTOR_STATE_DELETING is excluded. A record-less actor whose state is
// unspecified (zero value, or no status at all) must still be counted, so
// the probe fails closed on anything it cannot positively classify.
func TestSubstrateRestart_RecordlessActors_CountsUnspecifiedState(t *testing.T) {
	cases := []struct {
		name   string
		status *ateapipb.ActorStatus
	}{
		{"explicit UNSPECIFIED", &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_UNSPECIFIED}},
		{"nil status", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, fc, _, closeServer := newTestSubstrateHarness(t, &callRecorder{})
			defer closeServer()
			fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
				return &ateapipb.ListActorsResponse{
					Actors: []*ateapipb.Actor{probeEdgeActor("unknown-state", "uid-unknown", tc.status)},
				}, nil
			}

			_, actors, err := rt.RecordlessActors(context.Background(), probeEdgeProjectID)
			names := recordlessNames(actors)
			if err != nil {
				t.Fatalf("RecordlessActors() error = %v", err)
			}
			if len(names) != 1 || names[0] != "unknown-state" {
				t.Errorf("RecordlessActors() = %v, want [unknown-state]; only DELETING may be excluded", names)
			}
		})
	}
}
