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

package hub

// TopicEvent is published on project.<id>.chat.topic when a topic is
// created, updated, or deleted. It carries the action and the full
// topic snapshot so subscribers can update their local state without
// a subsequent REST fetch.
type TopicEvent struct {
	Action string       `json:"action"` // "created", "updated", "deleted"
	Topic  WebChatTopic `json:"topic"`
}
