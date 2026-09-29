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
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/third_party/ateapipb"
)

// RecordlessActors reads substrateAgentRecords while Run/Delete may write
// it from other request goroutines. Under -race, this fails if the read is
// not made under substrateAgentStateMu.
func TestSubstrateRestart_RecordlessActors_ConcurrentWithRecordWrites(t *testing.T) {
	rt, fc, _, closeServer := newTestSubstrateHarness(t, &callRecorder{})
	defer closeServer()

	const projectID = "550e8400-e29b-41d4-a716-446655440000"
	const atespace = "scion-550e8400-e29"
	fc.listActors = func(*ateapipb.ListActorsRequest) (*ateapipb.ListActorsResponse, error) {
		return &ateapipb.ListActorsResponse{Actors: []*ateapipb.Actor{{
			Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: "a", Uid: "uid-a"},
			Status:   &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
		}}}, nil
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			substrateAgentStateMu.Lock()
			substrateAgentRecords[fmt.Sprintf("uid-%d", i)] = &substrateAgentRecord{}
			substrateAgentStateMu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if _, _, err := rt.RecordlessActors(context.Background(), projectID); err != nil {
				t.Errorf("RecordlessActors() error = %v", err)
				return
			}
		}
	}()
	wg.Wait()
}
