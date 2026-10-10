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
	"fmt"

	"github.com/spf13/cobra"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// Assign status on the service account lists (ptone/scion#3329 phase 4c):
// --profile and --broker ask the Hub whether each account is mapped on that
// broker profile and add an ASSIGN column. Nothing is filtered: an account
// whose state is unknown is listed with its reason.

var (
	saListProfile string
	saListBroker  string
)

func addSAAssignStatusFlags(c *cobra.Command) {
	c.Flags().StringVar(&saListProfile, "profile", "",
		"Show whether each account is mapped on this broker profile (adds an ASSIGN column)")
	c.Flags().StringVar(&saListBroker, "broker", "",
		"Broker for the ASSIGN column (default: the broker agent creation would pick)")
}

func saAssignStatusRequested() bool {
	return saListProfile != "" || saListBroker != ""
}

func setSAAssignStatusOptions(opts *hubclient.ListGCPServiceAccountsOptions) {
	if opts == nil {
		return
	}
	opts.Profile = saListProfile
	opts.Broker = saListBroker
}

// saAssignLast renders the table's last column: base alone, or base padded
// to the VERIFIED/MAPPED width followed by the ASSIGN cell, so the output
// without the flags is unchanged.
func saAssignLast(assign bool, base, cell string) string {
	if !assign {
		return base
	}
	return fmt.Sprintf("%-8s  %s", base, cell)
}

// saAssignColumn renders the ASSIGN cell. "-" means the Hub sent no status
// (an older Hub).
func saAssignColumn(st *hubclient.GCPServiceAccountAssignStatus) string {
	if st == nil {
		return "-"
	}
	switch st.State {
	case "mapped":
		return "mapped"
	case "not_mapped":
		return "not mapped"
	case "not_required":
		return "not required"
	case "unknown":
		if st.Reason != "" {
			return "unknown (" + st.Reason + ")"
		}
		return "unknown"
	default:
		return st.State
	}
}

// printSAAssignFooter names the broker profile the ASSIGN column describes
// and what "mapped" does and does not mean.
func printSAAssignFooter(assign bool, sas []hubclient.GCPServiceAccount) {
	if !assign {
		return
	}
	var first *hubclient.GCPServiceAccountAssignStatus
	for i := range sas {
		if sas[i].AssignStatus != nil {
			first = sas[i].AssignStatus
			break
		}
	}
	fmt.Println()
	if first == nil {
		fmt.Println("ASSIGN: the Hub did not return an assign status (it may predate this feature).")
		return
	}
	broker, profile := first.BrokerName, first.Profile
	if broker == "" {
		broker = "(none)"
	}
	if profile == "" {
		profile = "(none)"
	}
	fmt.Printf("ASSIGN: broker %s, profile %s, from the broker's latest service account report.\n", broker, profile)
	fmt.Println("mapped means a Kubernetes service account is mapped; it does not mean ready: the Workload Identity IAM binding is not checked.")
	if first.Reason == "no_broker" && first.Message != "" {
		fmt.Println(first.Message)
	}
}
