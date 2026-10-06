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

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func TestIsPluginBroker(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{name: "nil labels", labels: nil, want: false},
		{name: "unrelated labels", labels: map[string]string{"scion.io/broker-type": "docker"}, want: false},
		{name: "plugin label", labels: map[string]string{"scion.io/plugin": "telegram"}, want: true},
		{name: "plugin label with empty value", labels: map[string]string{"scion.io/plugin": ""}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &store.RuntimeBroker{Labels: tt.labels}
			if got := isPluginBroker(b); got != tt.want {
				t.Errorf("isPluginBroker() = %v, want %v", got, tt.want)
			}
		})
	}
}
