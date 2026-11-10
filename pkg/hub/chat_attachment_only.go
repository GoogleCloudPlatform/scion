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

import "github.com/GoogleCloudPlatform/scion/pkg/store"

// An attachment-only chat message (ptone/scion#6146) is stored with an
// empty body. A soft-deleted message also has an empty body (its content
// is redacted on delete), so readers tell the two apart by the deleted
// flag of the message's extension row, never by the empty body alone.

// chatAttachmentOnlyPreview is the text shown where a text excerpt of an
// attachment-only message is expected. It matches starExcerpt.
const chatAttachmentOnlyPreview = "Attachment"

// chatHistoryMessageDeleted reports whether msg is soft-deleted. exts holds
// the extension rows read for the page; extsKnown says they were read. When
// they could not be read, an empty body is taken as deleted, so a deleted
// message is never shown as live.
func chatHistoryMessageDeleted(msg store.Message, exts map[string]*WebChatMessageExt, extsKnown bool) bool {
	if !extsKnown {
		return msg.Msg == ""
	}
	ext, ok := exts[msg.ID]
	return ok && ext != nil && ext.DeletedAt != nil
}

// errMentionCopyNeedsText is the mention result error of a message without
// text whose mention copy would carry nothing: processMentions copies the
// text only, and fanOutAgentMentions copies the text plus the parent's
// artifact attachments, so with neither the copy is not made.
const errMentionCopyNeedsText = "a message without text is not copied to mentioned agents"
