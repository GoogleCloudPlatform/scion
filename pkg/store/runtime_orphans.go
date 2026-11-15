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

package store

import "time"

// Kinds of a RuntimeOrphan.
const (
	// RuntimeOrphanKindInstance is a runtime entry (container, pod or
	// Cloud Run instance) a broker heartbeat reported.
	RuntimeOrphanKindInstance = "instance"
	// RuntimeOrphanKindNFSTree is reserved for orphaned agent storage trees.
	RuntimeOrphanKindNFSTree = "nfs_tree"
)

// RuntimeOrphan is a runtime object no Hub agent owns (ptone/scion#7930).
// ProjectID and AgentID are the values the broker reported; either may be
// unknown to the Hub. The Action fields are reserved for a later phase.
type RuntimeOrphan struct {
	ID                string     `json:"id"`
	Kind              string     `json:"kind"`
	BrokerID          string     `json:"brokerId"`
	RuntimeTarget     string     `json:"runtimeTarget,omitempty"`
	ProjectID         string     `json:"projectId,omitempty"`
	Slug              string     `json:"slug,omitempty"`
	AgentID           string     `json:"agentId,omitempty"`
	RunID             string     `json:"runId,omitempty"`
	Reason            string     `json:"reason"`
	ObservedPhase     string     `json:"observedPhase,omitempty"`
	FirstSeenAt       time.Time  `json:"firstSeenAt"`
	LastSeenAt        time.Time  `json:"lastSeenAt"`
	MissedInventories int        `json:"missedInventories"`
	ActionState       string     `json:"actionState"`
	ActionAt          *time.Time `json:"actionAt,omitempty"`
	ActionError       string     `json:"actionError,omitempty"`
}

// RuntimeOrphanFilter selects runtime orphans; empty fields match all.
type RuntimeOrphanFilter struct {
	BrokerID  string
	ProjectID string
	Kind      string
}

// RuntimeOrphanCount is the number of runtime orphans of one broker, kind
// and reason.
type RuntimeOrphanCount struct {
	BrokerID string
	Kind     string
	Reason   string
	Count    int
}
