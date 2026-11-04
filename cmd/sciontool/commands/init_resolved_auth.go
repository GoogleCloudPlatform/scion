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

package commands

import (
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/hooks"
	"github.com/GoogleCloudPlatform/scion/pkg/sciontool/log"
	"github.com/GoogleCloudPlatform/scion/pkg/stagedsecrets"
)

// resolvedAuthEnvRoot is the in-memory volume's mount point that holds the
// resolved-auth carry file (stagedsecrets.ResolvedAuthEnvFile); a variable
// so tests can point it at a temporary directory.
var resolvedAuthEnvRoot = hooks.DefaultHarnessDirsRoot

// takeResolvedAuthEnv reads and removes the resolved-auth carry file in
// root (stagedsecrets.TakeResolvedAuthEnv) and returns the entries the
// harness child may receive: a reserved or invalid name is dropped here
// again (stagedsecrets.ExportableResolvedAuthEnv), so an entry can never
// set the staged-token marker or a runtime-owned name. It returns nil when
// nothing was staged. Only counts are logged, never a name's value.
func takeResolvedAuthEnv(root string) (map[string]string, error) {
	env, err := stagedsecrets.TakeResolvedAuthEnv(root)
	if err != nil {
		return nil, err
	}
	kept, dropped := stagedsecrets.ExportableResolvedAuthEnv(env)
	if dropped > 0 {
		log.Error("Dropped %d staged resolved-auth env entr(ies) with a reserved or invalid name", dropped)
	}
	if len(kept) == 0 {
		return nil, nil
	}
	log.Info("Staged resolved-auth env for the harness: %d entr(ies)", len(kept))
	return kept, nil
}
