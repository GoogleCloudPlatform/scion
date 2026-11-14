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

package api

import "fmt"

// Persistence is an agent's choice of environment lifetime, fixed at
// create (ptone/scion#7780). Empty means PersistencePersistent.
type Persistence string

const (
	// PersistencePersistent keeps the agent's home and workspace across
	// stop, suspend and resume. It is the default.
	PersistencePersistent Persistence = "persistent"
	// PersistenceEphemeral gives the agent a fresh home and workspace at
	// every start; nothing it writes is kept once it stops.
	PersistenceEphemeral Persistence = "ephemeral"
)

// Validate returns an error unless p is empty, persistent or ephemeral.
func (p Persistence) Validate() error {
	switch p {
	case "", PersistencePersistent, PersistenceEphemeral:
		return nil
	}
	return fmt.Errorf("invalid persistence %q: must be persistent or ephemeral", string(p))
}

// IsEphemeral reports whether p is PersistenceEphemeral.
func (p Persistence) IsEphemeral() bool { return p == PersistenceEphemeral }
