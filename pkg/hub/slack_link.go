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
	"github.com/GoogleCloudPlatform/scion/pkg/ent/chatlinkcode"
)

// SlackLinkService manages pending Slack account link codes.
// When a ChatLinkStore is set, codes are persisted in the database so they
// survive across Hub instances. Otherwise, an in-memory map is used (single-node only).
type SlackLinkService struct {
	*chatLinkService
}

// NewSlackLinkService creates a new SlackLinkService and starts
// a background goroutine that periodically removes expired entries.
func NewSlackLinkService() *SlackLinkService {
	return &SlackLinkService{
		chatLinkService: newChatLinkService(chatlinkcode.ProviderSlack, "Slack"),
	}
}

// GetStatusBySlackUser returns the linking status for a given Slack user ID.
func (s *SlackLinkService) GetStatusBySlackUser(slackUserID string) (status, userID, userEmail string) {
	return s.GetStatusByUser(slackUserID)
}
