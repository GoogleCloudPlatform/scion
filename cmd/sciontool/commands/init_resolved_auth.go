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
	"errors"

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
// may receive. nonce is the marker's value: the nonce of the carry file
// this init's staging pass wrote, or "" when it wrote none.
//
// With a nonce, the file is read and removed (stagedsecrets.TakeResolvedAuthEnv)
// and a reserved or invalid name is then dropped again
// (stagedsecrets.ExportableResolvedAuthEnv), so an entry can never set the
// staged-token marker or a runtime-owned name. A file whose nonce differs
// was not written by that pass: it is removed, nothing is exported and the
// start continues.
//
// Without a nonce, a file found there was not written by this init: it is
// removed without being read, and that path never fails the start (a
// removal problem is logged), so a runtime that never stages entries
// cannot be stopped by what it finds there.
//
// It returns nil when nothing is exported. Only counts and conditions are
// logged, never a path, a value or the nonce.
func takeResolvedAuthEnv(root, nonce string) (map[string]string, error) {
	if nonce == "" {
		removed, err := stagedsecrets.DiscardResolvedAuthEnv(root)
		if err != nil {
			_ = err // the error names a path; only the condition is logged
			log.Error("Could not check or remove a resolved-auth carry file this init did not stage; it is not read and the start continues")
			return nil, nil
		}
		if removed {
			log.Error("Removed 1 resolved-auth carry file this init did not stage, without reading it")
		}
		return nil, nil
	}
	env, err := stagedsecrets.TakeResolvedAuthEnv(root, nonce)
	if errors.Is(err, stagedsecrets.ErrResolvedAuthEnvNonceMismatch) {
		log.Error("Removed a resolved-auth carry file that does not belong to this start, without exporting it")
		return nil, nil
	}
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
