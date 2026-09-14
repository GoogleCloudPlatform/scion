// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

func runHubAllOrOne(
	args []string,
	all bool,
	missingTargetError string,
	conflictingTargetError string,
	runAll func(context.Context, hubclient.Client) error,
	runOne func(context.Context, hubclient.Client, string) error,
) error {
	hasTarget := len(args) > 0
	if !hasTarget && !all {
		return errors.New(missingTargetError)
	}
	if hasTarget && all {
		return errors.New(conflictingTargetError)
	}

	_, client, err := requireHubClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if all {
		return runAll(ctx, client)
	}
	return runOne(ctx, client, args[0])
}
