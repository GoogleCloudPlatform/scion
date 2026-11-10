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

package brokerconvert

import (
	"context"
	"strings"
)

// Status reports the journal and each instance row's live state on the
// Hub. It is read-only: it takes no lock and writes nothing, and it sends
// GET requests only. It reads the journal and the Hub rows, never the
// settings file, so its report carries step states and IDs only.
func Status(ctx context.Context, env *Env) (*Report, error) {
	j, err := LoadJournal(env.GlobalDir)
	if err != nil {
		return refused(ModeStatus, nil, refuse(RefuseLocalWrite))
	}
	if j == nil {
		return refused(ModeStatus, nil, refuse(RefuseNoJournal))
	}
	rep := reportFrom(ModeStatus, j)
	if env.Client == nil || j.State == JournalRolledBack {
		return rep, nil
	}
	allOnline := len(rep.Instances) > 0
	for i := range rep.Instances {
		in := &rep.Instances[i]
		if in.BrokerID == "" {
			allOnline = false
			continue
		}
		row, err := env.Client.RuntimeBrokers().Get(ctx, in.BrokerID)
		if err != nil || row == nil {
			// Not visible or not reachable: the line shows no online state.
			allOnline = false
			continue
		}
		online := strings.EqualFold(row.Status, "online")
		in.Online = &online
		if !online {
			allOnline = false
		}
	}
	rep.Switched = j.State == JournalApplied && allOnline
	return rep, nil
}
