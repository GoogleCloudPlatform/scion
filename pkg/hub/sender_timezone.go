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

import (
	"context"
	"maps"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// stampSenderTimezone returns md with messages.MetadataSenderTimezone set to
// the time zone in the sender's user profile, as stored, when the sender is a
// human user (a "user:" sender whose senderID names a user with a profile
// time zone). Otherwise it returns md without that key: the profile has no
// time zone, the user cannot be read, or the sender is not a human user. md
// must already be stripped of hub-reserved keys
// (messaging.StripReservedMetadata). It never mutates md.
func (s *Server) stampSenderTimezone(ctx context.Context, md map[string]string, sender, senderID string) map[string]string {
	tz := ""
	if strings.HasPrefix(sender, "user:") && senderID != "" {
		if user, err := s.store.GetUser(ctx, senderID); err == nil && user != nil && user.Preferences != nil {
			tz = user.Preferences.Timezone
		}
	}
	if tz == "" {
		if _, ok := md[messages.MetadataSenderTimezone]; !ok {
			return md
		}
		out := maps.Clone(md)
		delete(out, messages.MetadataSenderTimezone)
		return out
	}
	out := make(map[string]string, len(md)+1)
	maps.Copy(out, md)
	out[messages.MetadataSenderTimezone] = tz
	return out
}
