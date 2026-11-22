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

import "strings"

// publishActionUpdated emits a live card update on
// user.<id>.chat.<kind>.updated (for DM threads, once per user participant) or
// project.<projectID>.chat.<kind>.updated (for project threads). For
// kind=="decision", the topic and payload match chat.decision.updated
// byte-for-byte.
func (s *Server) publishActionUpdated(kind, projectID, messageID, threadID string, view any) {
	if s == nil || s.events == nil || kind == "" {
		return
	}
	evt := map[string]interface{}{
		kind:        view,
		"messageId": messageID,
		"threadId":  threadID,
	}
	suffix := ".chat." + kind + ".updated"
	if strings.HasPrefix(threadID, "dm:") {
		parts := strings.Split(threadID, ":")
		if len(parts) >= 5 {
			for i, id := range []string{parts[2], parts[4]} {
				peerKind := parts[1]
				if i == 1 {
					peerKind = parts[3]
				}
				if peerKind == "user" && id != "" {
					s.events.PublishRaw("user."+id+suffix, evt)
				}
			}
		}
		return
	}
	if projectID != "" {
		s.events.PublishRaw("project."+projectID+suffix, evt)
	}
}
