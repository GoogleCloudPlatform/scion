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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

// hubSecretPendingCmd is the parent of the pending secret commands.
var hubSecretPendingCmd = &cobra.Command{
	Use:   "pending",
	Short: "Review secrets your agents saved for your account",
	Long: `Review user secrets written by your agents.

A user-scope secret an agent saves (for example a credential captured after
you log in inside an agent) is not delivered to any agent until you approve
it. Pending secrets expire after 7 days.

Values are never shown. Approving writes the secret to your user scope, where
it is delivered to your agents in every project (and to the agents they
create, unless you approve with --no-progeny).

Examples:
  scion hub secret pending list
  scion hub secret pending approve <ID>
  scion hub secret pending approve <ID> --no-progeny
  scion hub secret pending reject <ID>`,
}

func init() {
	hubSecretCmd.AddCommand(hubSecretPendingCmd)
}

// pendingSecretClient returns a Hub client and a request context for the
// pending secret commands. The pending endpoints always act on the calling
// user's own pending secrets, so no scope is resolved.
func pendingSecretClient() (hubclient.Client, context.Context, context.CancelFunc, error) {
	_, client, _, err := loadHubClientUAT()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	return client, ctx, cancel, nil
}
