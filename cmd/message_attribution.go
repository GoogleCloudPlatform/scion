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

package cmd

import (
	"context"
	"net/url"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// userLabeler returns the name a user ID is shown by.
type userLabeler func(userID string) string

// newUserLabeler returns a userLabeler that looks each user up on the hub
// once and shows the user's display name, else the email, else the ID. A
// failed lookup (not found, not permitted, unreachable) shows the ID. The
// hub is asked only when the labeler is called, so a listing with no
// delegated message makes no extra request.
func newUserLabeler(ctx context.Context, client hubclient.Client) userLabeler {
	labels := map[string]string{}
	return func(userID string) string {
		if label, ok := labels[userID]; ok {
			return label
		}
		label := userID
		if u, err := client.Users().Get(ctx, url.PathEscape(userID)); err == nil && u != nil {
			switch {
			case u.DisplayName != "":
				label = u.DisplayName
			case u.Email != "":
				label = u.Email
			}
		}
		labels[userID] = label
		return label
	}
}

// messageAuthorizedBy returns the attribution line of a message sent with
// an agent delegated credential, "authorized by <issuer> via delegation
// <grant>" (.design/agent-delegation.md §14.5), or "" for every other
// message. The message's sender stays the agent; the issuer is shown by
// label, or by ID when label is nil.
func messageAuthorizedBy(msg *store.Message, label userLabeler) string {
	if msg == nil || msg.AuthorizingUserID == "" {
		return ""
	}
	issuer := msg.AuthorizingUserID
	if label != nil {
		issuer = label(issuer)
	}
	line := "authorized by " + issuer
	if msg.DelegationGrantID != "" {
		line += " via delegation " + msg.DelegationGrantID
	}
	return line
}
