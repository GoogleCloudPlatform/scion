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
	"strings"
	"text/tabwriter"

	"github.com/GoogleCloudPlatform/scion/pkg/clitime"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/systemone/provider"
	"github.com/spf13/cobra"
)

type adminSystemOneListFlags struct {
	adapter string
	enabled bool
	limit   int
	cursor  string
}

var adminSystemOneListOpts adminSystemOneListFlags

var adminSystemOneInstanceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List Hub System One provider instances",
	Long: `List configured System One provider instances on the Hub.

Outputs credential-free metadata for each instance (secret values are never
returned).`,
	Args: cobra.NoArgs,
	RunE: runAdminSystemOneInstanceList,
}

func init() {
	f := adminSystemOneInstanceListCmd.Flags()
	f.StringVar(&adminSystemOneListOpts.adapter, "adapter", "", "Filter by provider adapter name")
	f.BoolVar(&adminSystemOneListOpts.enabled, "enabled", false, "Filter by enabled state (--enabled=true or --enabled=false)")
	f.IntVar(&adminSystemOneListOpts.limit, "limit", 0, "Maximum instances to return (1..500)")
	f.StringVar(&adminSystemOneListOpts.cursor, "cursor", "", "Pagination cursor from a previous list response")

	adminSystemOneInstanceCmd.AddCommand(adminSystemOneInstanceListCmd)
}

func runAdminSystemOneInstanceList(cmd *cobra.Command, _ []string) error {
	opts := &hubclient.ListSystemOneInstancesOptions{
		Adapter: strings.TrimSpace(adminSystemOneListOpts.adapter),
		Cursor:  strings.TrimSpace(adminSystemOneListOpts.cursor),
		Limit:   adminSystemOneListOpts.limit,
	}
	if cmd.Flags().Changed("enabled") {
		enabled := adminSystemOneListOpts.enabled
		opts.Enabled = &enabled
	}

	svc, ctx, cancel, err := systemOneAdminService(cmd, adminSystemOneDefaultTimeout)
	if err != nil {
		return err
	}
	defer cancel()

	resp, err := svc.ListInstances(ctx, opts)
	if err != nil {
		return err
	}

	if isJSONOutput() {
		return writeJSONTo(cmd.OutOrStdout(), resp)
	}

	out := cmd.OutOrStdout()
	if len(resp.Instances) == 0 {
		_, _ = fmt.Fprintln(out, "No System One instances configured.")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tADAPTER\tMODEL\tENDPOINT\tAUTH\tENABLED\tSECRET\tSCOPES\tQUOTA PROJECT\tCREDENTIAL\tUPDATED")
	for _, inst := range resp.Instances {
		model := inst.Model
		if model == "" {
			model = "-"
		}
		authKind := inst.Auth.Kind
		if authKind == "" {
			authKind = "-"
		}
		secretName := inst.Auth.SecretName
		if inst.Auth.SecretRef != nil && inst.Auth.SecretRef.Name != "" {
			if inst.Auth.SecretRef.Scope != "" {
				secretName = inst.Auth.SecretRef.Name + " (" + inst.Auth.SecretRef.Scope + ")"
			} else {
				secretName = inst.Auth.SecretRef.Name
			}
		}
		if secretName == "" {
			secretName = "-"
		}
		scopes := "-"
		if len(inst.Auth.Scopes) > 0 {
			scopes = strings.Join(inst.Auth.Scopes, ",")
		}
		quotaProj := inst.Auth.QuotaProject
		if quotaProj == "" && inst.Auth.Kind == string(provider.AuthKindGoogleAccessToken) {
			quotaProj = provider.FormatQuotaProjectProvenance(inst.Options[provider.OptionQuotaProject])
		}
		if quotaProj == "" {
			quotaProj = "-"
		}
		credInfo := inst.Auth.CredentialType
		if credInfo == "" {
			credInfo = "-"
		} else if inst.Auth.ServiceAccountEmail != "" {
			credInfo = credInfo + " (" + inst.Auth.ServiceAccountEmail + ")"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%t\t%s\t%s\t%s\t%s\t%s\n",
			inst.Name,
			inst.Adapter,
			model,
			inst.Endpoint,
			authKind,
			inst.Enabled,
			secretName,
			scopes,
			quotaProj,
			credInfo,
			clitime.Format(inst.UpdatedAt, clitime.Full),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if resp.NextCursor != "" {
		_, _ = fmt.Fprintf(out, "\nNext cursor: %s\n", resp.NextCursor)
	}
	return nil
}
