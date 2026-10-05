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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Join reports the broker's active profile, including an empty one; with
// no settings it reports nothing, so the hub keeps what it has.
func TestBrokerRegistrationDefaultProfile(t *testing.T) {
	assert.Nil(t, brokerRegistrationDefaultProfile(nil))

	got := brokerRegistrationDefaultProfile(&config.Settings{ActiveProfile: "k8s"})
	require.NotNil(t, got)
	assert.Equal(t, "k8s", *got)

	got = brokerRegistrationDefaultProfile(&config.Settings{})
	require.NotNil(t, got)
	assert.Equal(t, "", *got)
}
