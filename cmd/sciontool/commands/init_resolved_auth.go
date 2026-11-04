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

// envResolvedAuthStaged marks, across init's environ-purge re-exec, that
// this init's staging pass wrote the resolved-auth carry file. The
// runtimes reserve the name (no container environment supplies it).
const envResolvedAuthStaged = stagedsecrets.ResolvedAuthStagedEnvVar

// takeResolvedAuthEnv returns the resolved-auth entries the harness child
// may receive. Only when staged is set (this init's staging pass wrote the
// carry file) is the file read and removed (stagedsecrets.TakeResolvedAuthEnv),
// and a reserved or invalid name is then dropped again
// (stagedsecrets.ExportableResolvedAuthEnv), so an entry can never set the
// staged-token marker or a runtime-owned name. Without staged, a file found
// there was not written by this init: it is removed without being read.
// It returns nil when nothing is exported. Only counts are logged, never a
// value.
func takeResolvedAuthEnv(root string, staged bool) (map[string]string, error) {
	if !staged {
		removed, err := stagedsecrets.DiscardResolvedAuthEnv(root)
		if err != nil {
			return nil, err
		}
		if removed {
			log.Error("Removed 1 resolved-auth carry file this init did not stage, without reading it")
		}
		return nil, nil
	}
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
