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

package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/GoogleCloudPlatform/scion/pkg/provision"
)

// Home reset (a harness switch, ptone/scion#7757).
//
// A reprovision with StartOptions.ResetHome moves the agent's home aside,
// in the home's own parent, as removingHomePrefix+<reset ID>, and records
// the reset ID in homeResetRecordFile in the agent directory. The
// reprovision then composes a fresh home exactly as a create does. The
// moved-aside directory is purged in the background, again by the
// broker's startup sweep, and by agent delete.

// homeResetRecordFile is the per-agent file, in the agent directory next
// to home-storage.json and outside the agent home, that names the latest
// reset of the agent's home. It persists: runtimes whose home is kept
// elsewhere (an NFS leaf) compare their own copy against it.
const homeResetRecordFile = "home-reset.json"

// removingHomePrefix starts the name of a home moved aside by a reset. It
// is in the home's parent, which is the agent directory for an in-project
// home and a directory outside it for split storage. The prefix also
// starts with provision's moved-aside prefix (".removing-"), so a purge of
// an agent directory's moved-aside workspaces removes it too.
const removingHomePrefix = ".removing-home-"

// ErrHomeResetApplied marks a Reprovision failure that happened after the
// agent's home was moved aside: the previous home is gone. A Reprovision
// failure without it left the home intact.
var ErrHomeResetApplied = errors.New("the agent home was already reset")

// homeResetAppliedError wraps a Reprovision failure after the home reset so
// that errors.Is matches both ErrHomeResetApplied and the original error.
type homeResetAppliedError struct{ err error }

func (e homeResetAppliedError) Error() string {
	return e.err.Error() + " (the agent home was already reset)"
}
func (e homeResetAppliedError) Unwrap() []error {
	return []error{e.err, ErrHomeResetApplied}
}

// homeResetRecord is the content of homeResetRecordFile.
type homeResetRecord struct {
	ResetID string `json:"reset_id"`
}

// purgeResetHomeResidue deletes every home a reset moved aside in
// homeParent (provision.PurgePrefixedEntries, which stays inside the
// directory it opened). A missing homeParent is not an error. Nothing
// refers to a moved-aside home, so it runs without a lock.
func purgeResetHomeResidue(homeParent string) error {
	return provision.PurgePrefixedEntries(homeParent, removingHomePrefix)
}

// purgeResetHomeResidueAsync purges homeParent in the background and logs a
// failure (for example files owned by a container user namespace). The
// moved-aside home is outside every mount, so a leftover is inert until the
// next purge. A variable so tests can run it inline.
var purgeResetHomeResidueAsync = func(ctx context.Context, homeParent string) {
	log := storageAuditLogger(ctx)
	go func() {
		if err := purgeResetHomeResidue(homeParent); err != nil {
			log.Warn("Home reset: could not purge the moved-aside agent home", "dir", homeParent, "error", err)
		}
	}()
}

// PurgeResetHomeResidueUnder deletes the homes resets moved aside for the
// agents of the projects kept in the Scion global directory globalDir: the
// Hub-managed projects (<globalDir>/projects/<slug>/.scion/agents/<agent>)
// and the split-storage project configurations
// (<globalDir>/project-configs/<dir>/.scion/agents/<agent>). The broker
// runs it once at startup, for residue an interrupted purge left. Homes of
// agents in other project checkouts are purged by the agent's next reset or
// delete. Failures are logged and never stop the sweep.
func PurgeResetHomeResidueUnder(globalDir string, log *slog.Logger) {
	if globalDir == "" {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	for _, pattern := range []string{
		filepath.Join(globalDir, "projects", "*", ".scion", "agents", "*"),
		filepath.Join(globalDir, "project-configs", "*", ".scion", "agents", "*"),
	} {
		dirs, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, dir := range dirs {
			if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
				continue
			}
			if err := purgeResetHomeResidue(dir); err != nil {
				log.Warn("Home reset: startup sweep could not purge a moved-aside agent home", "dir", dir, "error", err)
			}
		}
	}
}
