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
	"fmt"
	"io"
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

Values are shown only by 'reveal', which the Hub records in its audit log.
Approving writes the secret to your user scope, where it is delivered to your
agents in every project (and to the agents they create, unless you approve
with --no-progeny).

An agent can hold at most 10 pending secrets and your account at most 50;
further saves are refused until you approve or reject some.

Examples:
  scion hub secret pending list
  scion hub secret pending show <ID>
  scion hub secret pending reveal <ID>
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

// pendingSecretHintTimeout bounds the pending secret lookup of the start
// hint, so a slow Hub never delays a start noticeably.
const pendingSecretHintTimeout = 2 * time.Second

// printPendingSecretHint writes "N secrets await approval" to w when the
// calling user has pending secrets. It is for the user's own terminal: it
// prints nothing inside an agent (agent mode, an agent container or a
// Hub-managed agent), and nothing when the lookup fails, for example on
// a credential the pending secret endpoints refuse.
func printPendingSecretHint(ctx context.Context, w io.Writer, client hubclient.Client) {
	if client == nil || contextDisabled(resolveMode()) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, pendingSecretHintTimeout)
	defer cancel()
	resp, err := client.Secrets().ListPending(ctx)
	if err != nil || resp == nil || len(resp.PendingSecrets) == 0 {
		return
	}
	n := len(resp.PendingSecrets)
	if n == 1 {
		_, _ = fmt.Fprintln(w, "1 secret awaits approval (scion hub secret pending list)")
		return
	}
	_, _ = fmt.Fprintf(w, "%d secrets await approval (scion hub secret pending list)\n", n)
}
