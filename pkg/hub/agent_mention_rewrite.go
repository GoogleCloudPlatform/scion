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

import "github.com/GoogleCloudPlatform/scion/pkg/messages"

// rewriteIncomingAgentMentions applies the shared mention rule
// (messages.RewriteAgentMentions) to a message body the hub has just
// accepted: an "@agent:<slug>" mention becomes "@<slug>". Each send path
// calls it once, before it stores the body or extracts mentions from it,
// so the stored body and the mentions come from the same text and web,
// chat and CLI senders behave the same.
//
// Code spans, fenced blocks and quoted lines are left unchanged, and the
// rule is idempotent, so a body the CLI already rewrote is returned as is.
func rewriteIncomingAgentMentions(body string) string {
	return messages.RewriteAgentMentions(body)
}
