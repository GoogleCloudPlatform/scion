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

package fswatcher

import "time"

// Action represents the type of filesystem event.
type Action string

const (
	ActionRead       Action = "read"
	ActionCreate     Action = "create"
	ActionModify     Action = "modify"
	ActionDelete     Action = "delete"
	ActionRenameFrom Action = "rename_from"
	ActionRenameTo   Action = "rename_to"
)

// Event is a single filesystem event attributed to an agent.
type Event struct {
	Timestamp time.Time `json:"ts"`
	AgentID   string    `json:"agent_id"`
	Action    Action    `json:"action"`
	Path      string    `json:"path"`
	Size      *int64    `json:"size,omitempty"`
}

// Config holds the watcher configuration parsed from CLI flags.
type Config struct {
	Project    string
	WatchDirs  []string
	LogFile    string
	LabelKey   string
	Ignore     []string
	FilterFile string
	Debounce   time.Duration
	CacheTTL   time.Duration
	Debug      bool
}
