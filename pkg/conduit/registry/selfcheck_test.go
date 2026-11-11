// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSelfCheck(t *testing.T) {
	probeErr := errors.New("connection refused")
	cases := []struct {
		name     string
		endpoint string
		probe    ProbeFunc
		wantErr  error
	}{
		{"success", "http://relay-0:9811", func(_ context.Context, url string) (string, error) {
			assert.Equal(t, "http://relay-0:9811", url)
			return "relay-0", nil
		}, nil},
		{"mismatch", "http://relay-0:9811", func(context.Context, string) (string, error) { return "relay-1", nil }, ErrSelfCheckMismatch},
		{"probe error", "http://relay-0:9811", func(context.Context, string) (string, error) { return "", probeErr }, probeErr},
		{"unaddressable", "", func(context.Context, string) (string, error) {
			t.Fatal("probe must not be called for an empty endpoint")
			return "", nil
		}, ErrUnaddressable},
		{"nil probe", "http://relay-0:9811", nil, ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SelfCheck(context.Background(), tc.endpoint, "relay-0", tc.probe)
			if tc.wantErr == nil {
				require.NoError(t, err)
				return
			}
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}
