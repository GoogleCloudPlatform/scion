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
	"errors"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateAllResources(t *testing.T) {
	targets := []validationTarget{
		{id: "pass-id", name: "passes"},
		{id: "issue-id", name: "has-issue"},
		{id: "error-id", name: "errors"},
	}

	var validated []string
	var validationErr error
	output := captureStdout(t, func() {
		validationErr = validateAllResources(context.Background(), "template", targets,
			func(_ context.Context, id string) (*hubclient.ValidationReport, error) {
				validated = append(validated, id)
				switch id {
				case "pass-id":
					return &hubclient.ValidationReport{}, nil
				case "issue-id":
					return &hubclient.ValidationReport{Issues: []hubclient.ValidationIssue{{Kind: "missing", Message: "file absent"}}}, nil
				default:
					return nil, errors.New("request failed")
				}
			})
	})

	require.EqualError(t, validationErr, "2 template(s) failed validation")
	assert.Equal(t, []string{"pass-id", "issue-id", "error-id"}, validated)
	assert.Contains(t, output, "PASS  passes")
	assert.Contains(t, output, "FAIL  has-issue")
	assert.Contains(t, output, "  - [missing] file absent")
	assert.Contains(t, output, "FAIL  errors  (error: request failed)")
	assert.Contains(t, output, "1 passed, 2 failed")
}

func TestValidateAllResourcesEmpty(t *testing.T) {
	var called bool
	var validationErr error
	output := captureStdout(t, func() {
		validationErr = validateAllResources(context.Background(), "harness-config", nil,
			func(context.Context, string) (*hubclient.ValidationReport, error) {
				called = true
				return nil, nil
			})
	})

	require.NoError(t, validationErr)
	assert.False(t, called)
	assert.Equal(t, "No harness-configs found.\n", output)
}
