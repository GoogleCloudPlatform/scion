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

package projectfs

import (
	"fmt"
	"path"
	"strings"
)

// Chat attachments projected into a shared dir (ptone/scion#5827): every surface
// that makes a file visible to the agents of a project (hub chat, the chat
// plugins, `scion message --attach`, terminal drag-drop) writes it below
// <shared dir>/.attachments/<channel>/<id>/, and every container path for
// such a file is computed here from the Mount's Target. Phase 3's Realize
// keeps Target unchanged for bind and pod mounts, so these paths stay valid
// when it lands.

// AttachmentsDir is the directory under a shared dir that holds projected
// attachments.
const AttachmentsDir = ".attachments"

// ScratchpadName is the conventional shared dir attachments are projected
// into.
const ScratchpadName = "scratchpad"

// Attachment channels: the per-surface directory under AttachmentsDir.
const (
	AttachmentChannelWebChat  = "_webchat"
	AttachmentChannelTerminal = "_web"
	AttachmentChannelDiscord  = "_discord"
	AttachmentChannelTelegram = "_telegram"
	AttachmentChannelGChat    = "_gchat"
	// AttachmentChannelUser is the CLI channel of a sender that is not an
	// agent; an agent sending with --attach uses its own slug.
	AttachmentChannelUser = "_user"
)

// validAttachmentComponent checks one path component of an attachment
// path: non-empty, not "." or "..", no separator, no NUL.
func validAttachmentComponent(what, s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\\\x00") {
		return fmt.Errorf("projectfs: invalid attachment %s %q", what, s)
	}
	return nil
}

// AttachmentRelDir returns ".attachments/<channel>/<id>", the directory of
// one attachment relative to the shared dir it is projected into. channel
// and id must each be a single path component.
func AttachmentRelDir(channel, id string) (string, error) {
	if err := validAttachmentComponent("channel", channel); err != nil {
		return "", err
	}
	if err := validAttachmentComponent("id", id); err != nil {
		return "", err
	}
	return path.Join(AttachmentsDir, channel, id), nil
}

// AttachmentContainerPath returns the container path of attachment file name
// projected into Mount m: <m.Target>/.attachments/<channel>/<id>/<name>.
// name must be a single path component.
func AttachmentContainerPath(m Mount, channel, id, name string) (string, error) {
	if m.Target == "" || !path.IsAbs(m.Target) {
		return "", fmt.Errorf("projectfs: mount %q has no absolute container target", m.Name)
	}
	rel, err := AttachmentRelDir(channel, id)
	if err != nil {
		return "", err
	}
	if err := validAttachmentComponent("name", name); err != nil {
		return "", err
	}
	return path.Join(m.Target, rel, name), nil
}

// SharedDirTargets returns the container paths shared dir name may be
// mounted at in a container whose workspace is containerWorkspace: the
// /scion-volumes/<name> form first, then the in-workspace form. A process
// inside an agent container that does not know the declaration (the CLI)
// probes them in this order.
func SharedDirTargets(name, containerWorkspace string) []string {
	return []string{
		SharedDirTarget(name, false, containerWorkspace),
		SharedDirTarget(name, true, containerWorkspace),
	}
}
