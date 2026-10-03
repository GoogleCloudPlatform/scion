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
	"errors"

	"github.com/spf13/cobra"
)

// gcpProjectFlagHint is shown when a user passes the old local --project flag
// (which used to mean the GCP project ID on `project service-accounts add`
// and on `hub secret migrate`) now that it resolves to the root --project/-g
// scion project selector instead. There is no alias for the old flag name: a
// local alias called "project" would recreate the shadowing this rename
// fixes.
const gcpProjectFlagHint = "--project now selects the scion project; pass the GCP project ID with --gcp-project"

// checkGCPProjectFlag detects the pre-rename invocation `... --project <gcp>`
// on a command with a required --gcp-project flag, and returns a targeted
// hint before cobra's required-flag check fires, since that check's generic
// "required flag(s) \"gcp-project\" not set" message gives no clue that
// --project changed meaning. Used as PreRunE on both `project
// service-accounts add` (cmd/project_service_accounts.go) and `hub secret
// migrate` (cmd/hub_secret_migrate.go); kept in its own file, rather than in
// either of those command files, since both depend on it.
func checkGCPProjectFlag(cmd *cobra.Command, args []string) error {
	if !cmd.Flags().Changed("gcp-project") && cmd.Flags().Changed("project") {
		return errors.New(gcpProjectFlagHint)
	}
	return nil
}
