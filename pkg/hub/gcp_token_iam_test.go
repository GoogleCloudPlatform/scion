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

	"github.com/stretchr/testify/assert"
)

// Compile-time check that IAMTokenGenerator implements GCPTokenGenerator.
var _ GCPTokenGenerator = (*IAMTokenGenerator)(nil)

func TestSAResourceName(t *testing.T) {
	got := saResourceName("agent@my-project.iam.gserviceaccount.com")
	assert.Equal(t, "projects/-/serviceAccounts/agent@my-project.iam.gserviceaccount.com", got)
}

func TestIAMTokenGenerator_ServiceAccountEmail(t *testing.T) {
	g := &IAMTokenGenerator{hubServiceAccountEmail: "hub@project.iam.gserviceaccount.com"}
	assert.Equal(t, "hub@project.iam.gserviceaccount.com", g.ServiceAccountEmail())
}

func TestIAMTokenGenerator_ServiceAccountEmail_Empty(t *testing.T) {
	g := &IAMTokenGenerator{}
	assert.Equal(t, "", g.ServiceAccountEmail())
}
