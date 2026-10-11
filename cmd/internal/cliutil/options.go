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

// Package cliutil holds helpers shared by the scion CLI commands that do not
// depend on the command tree itself.
package cliutil

import "context"

// RootOptions holds the values of scion's persistent (root) flags for one
// invocation, as normalized by the root command's PersistentPreRunE (for
// example --global sets ProjectPath to "global", and --non-interactive
// implies AutoConfirm). It is a plain value: commands receive a copy and
// tests construct one directly.
type RootOptions struct {
	ProjectPath    string // --project / -g
	GlobalMode     bool   // --global
	Profile        string // --profile / -p
	OutputFormat   string // --format
	HubEndpoint    string // --hub
	NoHub          bool   // --no-hub
	AutoConfirm    bool   // --yes / -y
	NonInteractive bool   // --non-interactive
	Debug          bool   // --debug
	DisplayTZ      string // --tz
	DisplayUTC     bool   // --utc
}

// JSONOutput reports whether --format json is in effect.
func (o RootOptions) JSONOutput() bool {
	return o.OutputFormat == "json"
}

type rootOptionsKey struct{}

// WithRootOptions returns a copy of ctx that carries opts. A nil ctx is
// treated as context.Background().
func WithRootOptions(ctx context.Context, opts RootOptions) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, rootOptionsKey{}, opts)
}

// RootOptionsFrom returns the RootOptions carried by ctx, and whether ctx
// carries any. A nil ctx carries none.
func RootOptionsFrom(ctx context.Context) (RootOptions, bool) {
	if ctx == nil {
		return RootOptions{}, false
	}
	opts, ok := ctx.Value(rootOptionsKey{}).(RootOptions)
	return opts, ok
}
