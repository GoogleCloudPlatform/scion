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
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

// saRemoveForce is --force on both `remove` commands (ptone/scion#4022).
var saRemoveForce bool

const saRemoveForceUsage = "Clear project and per-profile defaults that point at the account, then remove it (never clears the hub default)"

// removeServiceAccount deletes one registration and prints the Hub's impact
// report. On an in-use refusal it prints the report and returns an error
// telling the user what to do next. Shared by `scion project service-accounts
// remove` and `scion service-accounts remove`.
func removeServiceAccount(ctx context.Context, client hubclient.Client, ref hubclient.GCPServiceAccountRef, saID string, force, asJSON bool) error {
	res, err := client.GCPServiceAccounts().Delete(ctx, ref, &hubclient.DeleteGCPServiceAccountOptions{Force: force})
	if err != nil {
		if impact, ok := hubclient.GCPServiceAccountImpactFromError(err); ok {
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				_ = enc.Encode(map[string]interface{}{"deleted": false, "impact": impact})
			} else {
				printSAInUse(os.Stdout, saID, impact, force)
			}
			hub, other := blockingDefaults(impact)
			switch {
			case hub:
				return fmt.Errorf("service account %s is the hub default; a hub admin must change the hub default before it can be removed", saID)
			case other:
				return fmt.Errorf("service account %s is a default in projects you cannot change; an admin of those projects must change them before it can be removed", saID)
			}
			return fmt.Errorf("service account %s is in use by defaults; re-run with --force to clear them and remove it", saID)
		}
		return fmt.Errorf("failed to remove service account: %w", err)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	printSARemoved(os.Stdout, saID, res)
	return nil
}

// blockingDefaults reports whether a default --force cannot clear for this
// caller is the hub default, or one in a project the caller cannot change.
func blockingDefaults(impact *hubclient.GCPServiceAccountImpact) (hub, other bool) {
	for _, d := range impact.Defaults {
		if d.Clearable {
			continue
		}
		if d.Tier == "hub" {
			hub = true
		} else {
			other = true
		}
	}
	return hub, other
}

func describeSADefault(d hubclient.GCPServiceAccountImpactDefault) string {
	switch {
	case d.Tier == "hub":
		return "hub default"
	case d.Redacted && d.Tier == "profile":
		return "a per-profile default in another project"
	case d.Redacted:
		return "a project default in another project"
	}
	switch d.Tier {
	case "profile":
		return fmt.Sprintf("project %s, profile %q default", d.ProjectID, d.Profile)
	default:
		return fmt.Sprintf("project %s default", d.ProjectID)
	}
}

// printSAInUse prints a refused removal.
func printSAInUse(w io.Writer, saID string, impact *hubclient.GCPServiceAccountImpact, force bool) {
	_, _ = fmt.Fprintf(w, "Service account %s was not removed: defaults still point at it.\n", saID)
	_, _ = fmt.Fprintln(w, "Defaults:")
	for _, d := range impact.Defaults {
		note := ""
		switch {
		case d.Clearable:
		case d.Tier == "hub":
			note = " (not cleared by --force; a hub admin must change it)"
		default:
			note = " (not cleared by --force; an admin of that project must change it)"
		}
		_, _ = fmt.Fprintf(w, "  - %s%s\n", describeSADefault(d), note)
	}
	printSAImpactCommon(w, impact)
	hub, other := blockingDefaults(impact)
	switch {
	case hub || other:
		if hub {
			_, _ = fmt.Fprintln(w, "Ask a hub admin to change the hub default.")
		}
		if other {
			_, _ = fmt.Fprintln(w, "Ask an admin of the other projects to change their defaults.")
		}
		_, _ = fmt.Fprintln(w, "Then retry.")
	case !force:
		_, _ = fmt.Fprintln(w, "Re-run with --force to clear these defaults and remove the account.")
		_, _ = fmt.Fprintln(w, "A cleared 'assign' default becomes 'block': new agents get no GCP identity until a new default is set.")
	}
}

// printSARemoved prints a successful removal and the cleanup left to do.
func printSARemoved(w io.Writer, saID string, res *hubclient.DeleteGCPServiceAccountResult) {
	_, _ = fmt.Fprintf(w, "Removed service account %s\n", saID)
	if len(res.ClearedDefaults) > 0 {
		_, _ = fmt.Fprintln(w, "Cleared defaults ('assign' defaults are now 'block'):")
		for _, d := range res.ClearedDefaults {
			_, _ = fmt.Fprintf(w, "  - %s\n", describeSADefault(d))
		}
	}
	printSAImpactCommon(w, &res.Impact)
	if len(res.Impact.ManualCleanup) > 0 {
		_, _ = fmt.Fprintln(w, "Cleanup the hub cannot do:")
		for _, step := range res.Impact.ManualCleanup {
			_, _ = fmt.Fprintf(w, "  - %s\n", step)
		}
	}
}

func printSAImpactCommon(w io.Writer, impact *hubclient.GCPServiceAccountImpact) {
	if impact.AgentCount > 0 {
		_, _ = fmt.Fprintf(w, "Agents referencing it: %d (they do not block removal and fail at their next start until given another identity)\n", impact.AgentCount)
		for _, a := range impact.Agents {
			_, _ = fmt.Fprintf(w, "  - %s (%s) in project %s\n", a.Name, a.ID, a.ProjectID)
		}
		if len(impact.Agents) < impact.VisibleAgentCount {
			_, _ = fmt.Fprintf(w, "  ... and %d more\n", impact.VisibleAgentCount-len(impact.Agents))
		}
		if hidden := sumInts(impact.HiddenAgentCounts); hidden > 0 {
			_, _ = fmt.Fprintf(w, "  %d agent(s) you cannot see, in %d project(s)\n", hidden, len(impact.HiddenAgentCounts))
		}
	}
	if len(impact.BrokerMappings) > 0 {
		_, _ = fmt.Fprintln(w, "Broker profiles that map it:")
		for _, m := range impact.BrokerMappings {
			name := m.BrokerName
			if name == "" {
				name = m.BrokerID
			}
			_, _ = fmt.Fprintf(w, "  - %s/%s\n", name, m.Profile)
		}
	}
}

func sumInts(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return n
}
