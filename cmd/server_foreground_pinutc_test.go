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
	"log/slog"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestRunServerStart_PinsUTCBeforeAnyOtherWork asserts that runServerStart
// calls the UTC pin as the very first thing it does, before logging
// initialization or any other setup. It uses the pinProcessUTCFn and
// initServerLoggingFn seams rather than starting a real server: a real
// `scion server start` would open stores, bind ports and block, none of
// which this test wants to exercise.
func TestRunServerStart_PinsUTCBeforeAnyOtherWork(t *testing.T) {
	var calls []string

	origPin := pinProcessUTCFn
	origInit := initServerLoggingFn
	t.Cleanup(func() {
		pinProcessUTCFn = origPin
		initServerLoggingFn = origInit
	})

	pinProcessUTCFn = func() {
		calls = append(calls, "pin")
	}
	sentinelErr := errors.New("stop after logging init: seam for ordering test only")
	initServerLoggingFn = func(cmd *cobra.Command) ([]func(), *slog.Logger, *slog.Logger, error) {
		calls = append(calls, "initLogging")
		// Short-circuit runServerStart right after logging init, before it
		// touches config, stores or ports. The ordering assertion below only
		// needs the first two calls.
		return nil, nil, nil, sentinelErr
	}

	cmd := &cobra.Command{}
	err := runServerStart(cmd, nil)

	require.ErrorIs(t, err, sentinelErr)
	require.Equal(t, []string{"pin", "initLogging"}, calls, "runServerStart must pin UTC before any other work")
}
