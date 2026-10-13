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

package cliutil

import (
	"context"
	"testing"
)

func TestRootOptionsJSONOutput(t *testing.T) {
	for format, want := range map[string]bool{"": false, "plain": false, "json": true} {
		if got := (RootOptions{OutputFormat: format}).JSONOutput(); got != want {
			t.Errorf("JSONOutput() with format %q = %v, want %v", format, got, want)
		}
	}
}

func TestRootOptionsContextRoundTrip(t *testing.T) {
	want := RootOptions{ProjectPath: "global", GlobalMode: true, OutputFormat: "json", AutoConfirm: true}
	got, ok := RootOptionsFrom(WithRootOptions(context.Background(), want))
	if !ok || got != want {
		t.Fatalf("RootOptionsFrom() = %+v, %v; want %+v, true", got, ok, want)
	}
}

func TestRootOptionsFromMissing(t *testing.T) {
	//nolint:staticcheck // a nil context is what an unexecuted cobra command returns
	if _, ok := RootOptionsFrom(nil); ok {
		t.Error("RootOptionsFrom(nil) reported options")
	}
	if _, ok := RootOptionsFrom(context.Background()); ok {
		t.Error("RootOptionsFrom(Background) reported options")
	}
}

func TestWithRootOptionsNilParent(t *testing.T) {
	//nolint:staticcheck // a nil context is what an unexecuted cobra command returns
	ctx := WithRootOptions(nil, RootOptions{Profile: "p"})
	if got, ok := RootOptionsFrom(ctx); !ok || got.Profile != "p" {
		t.Fatalf("RootOptionsFrom() = %+v, %v; want Profile p", got, ok)
	}
}
