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
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/tunnelclient"
	"github.com/spf13/cobra"
)

var tunnelStatusJSON bool

var tunnelStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the open tunnels of this user's tunnel processes",
	Long: `Show the open tunnels of this user's running scion tunnel processes:
the hub endpoint, the credential identity (kind, subject and scope; never the
secret), the session ids and the open tunnels. With --hub, only tunnels to
that hub are shown. The command reads local state only and never prompts.`,
	Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return tunnelclient.Errorf(tunnelclient.ExitUsage, "tunnel status takes no arguments")
		}
		return nil
	},
	RunE: runTunnelStatus,
}

func init() {
	tunnelStatusCmd.Flags().BoolVar(&tunnelStatusJSON, "json", false, "Output JSON")
	tunnelStatusCmd.SetFlagErrorFunc(tunnelFlagError)
	tunnelCmd.AddCommand(tunnelStatusCmd)
}

func runTunnelStatus(cmd *cobra.Command, _ []string) error {
	store, err := tunnelStateStore()
	if err != nil {
		return tunnelclient.Errorf(tunnelclient.ExitError, "locating tunnel state: %v", err)
	}
	return writeTunnelStatus(cmd.OutOrStdout(), store, hubEndpoint, tunnelStatusJSON || outputFormat == "json")
}

// writeTunnelStatus lists the live records in store, merged per endpoint
// and credential identity, filtered to endpoint when it is set.
func writeTunnelStatus(w io.Writer, store tunnelclient.StateStore, endpoint string, asJSON bool) error {
	records, err := store.List()
	if err != nil {
		return tunnelclient.Errorf(tunnelclient.ExitError, "reading tunnel state: %v", err)
	}
	st := tunnelclient.MergeStatus(records, endpoint)
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(st); err != nil {
			return tunnelclient.Errorf(tunnelclient.ExitError, "writing status: %v", err)
		}
		return nil
	}
	if len(st.Entries) == 0 {
		_, _ = fmt.Fprintln(w, "No open tunnels.")
		return nil
	}
	for i, e := range st.Entries {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		_, _ = fmt.Fprintf(w, "Hub:        %s\n", e.Endpoint)
		_, _ = fmt.Fprintf(w, "Credential: %s %s (scope %s)\n", e.Credential.Kind, e.Credential.Subject, e.Credential.Scope)
		for _, id := range e.SessionIDs {
			_, _ = fmt.Fprintf(w, "Session:    %s\n", id)
		}
		if len(e.Tunnels) == 0 {
			_, _ = fmt.Fprintln(w, "No open tunnels.")
			continue
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "AGENT\tPROJECT\tKIND\tPORT\tOPENED\tAUTHZ DEADLINE")
		for _, t := range e.Tunnels {
			deadline := "-"
			if t.AuthzDeadline != nil {
				deadline = clitime.Format(*t.AuthzDeadline, clitime.Full)
			}
			port := "-"
			if t.Port != 0 {
				port = fmt.Sprint(t.Port)
			}
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.Agent, t.Project, t.Kind, port, clitime.Format(t.OpenedAt, clitime.Full), deadline)
		}
		_ = tw.Flush()
	}
	return nil
}
