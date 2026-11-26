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

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/procreap"
	"github.com/GoogleCloudPlatform/scion/pkg/trajectory/agydecode"
	"github.com/spf13/cobra"
)

func detectLocalAgyVersion(ctx context.Context) string {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	out, err := procreap.OutputManaged(exec.CommandContext(cctx, "agy", "--version"))
	if err != nil {
		return ""
	}
	return agydecode.NormalizeVersion(string(out))
}

func newInternalDecodeAgyDBCmd() *cobra.Command {
	var (
		dbPath     string
		format     string
		agyVersion string
		skipSteps  bool
	)

	cmd := &cobra.Command{
		Use:    "decode-agy-db",
		Short:  "Decode an Antigravity conversation SQLite database into canonical usage JSON",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(dbPath) == "" {
				return errors.New("--db is required")
			}
			if format != "json" {
				return fmt.Errorf("unsupported --format %q (only \"json\" is supported)", format)
			}

			ver := strings.TrimSpace(agyVersion)
			if ver == "" {
				ver = detectLocalAgyVersion(cmd.Context())
			}

			opts := agydecode.Options{
				AgyVersion: ver,
				SkipSteps:  skipSteps,
			}

			res, err := agydecode.DecodeFile(cmd.Context(), dbPath, opts)
			if err != nil {
				return err
			}

			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(res)
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", "", "Path to an Antigravity conversation .db file (required)")
	cmd.Flags().StringVar(&format, "format", "json", "Output format (json)")
	cmd.Flags().StringVar(&agyVersion, "agy-version", "", "Override detected agy version for version-pinning check")
	cmd.Flags().BoolVar(&skipSteps, "skip-steps", false, "Skip scanning the steps table (gen_metadata only)")
	_ = cmd.MarkFlagRequired("db")

	return cmd
}

func init() {
	internalCmd.AddCommand(newInternalDecodeAgyDBCmd())
}
