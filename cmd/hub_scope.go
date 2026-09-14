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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/spf13/cobra"
)

type hubScopeResolver func(*cobra.Command, *config.Settings) (string, string, error)

func resolveHubScope(
	cmd *cobra.Command,
	resolve hubScopeResolver,
) (hubclient.Client, string, string, context.Context, context.CancelFunc, error) {
	resolvedPath, _, err := config.ResolveProjectPath(projectPath)
	if err != nil {
		return nil, "", "", nil, nil, fmt.Errorf("failed to resolve project path: %w", err)
	}

	settings, err := config.LoadSettings(resolvedPath)
	if err != nil {
		return nil, "", "", nil, nil, fmt.Errorf("failed to load settings: %w", err)
	}

	client, err := getHubClient(settings)
	if err != nil {
		return nil, "", "", nil, nil, err
	}

	scope, scopeID, err := resolve(cmd, settings)
	if err != nil {
		return nil, "", "", nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	scopeID, err = resolveScopeID(ctx, client, scope, scopeID)
	if err != nil {
		cancel()
		return nil, "", "", nil, nil, err
	}

	return client, scope, scopeID, ctx, cancel, nil
}
