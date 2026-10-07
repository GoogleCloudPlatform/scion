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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// ErrContainerNotFound reports that an exec could not run because the
// agent's container no longer exists (for example, it was removed between
// the broker's lookup and the exec). An error wrapping it never wraps the
// CLI's *exec.ExitError, so callers that treat an exit error as the
// command's own result (ptone/scion#3470) do not mistake it for one.
var ErrContainerNotFound = errors.New("container not found")

// containerExecNotFound describes how a container CLI reports that the exec
// target does not exist: the CLI's own exit code for that failure, and its
// exact error line for the target id.
type containerExecNotFound struct {
	exitCode int
	line     func(quotedID string) string // regexp for one output line; quotedID is regexp.QuoteMeta(id)
}

var (
	// docker exec: the daemon's "No such container: <id>" (moby
	// errdefs.NotFound), printed by the CLI with or without the
	// "Error response from daemon: " prefix; the CLI exits 1.
	dockerExecNotFound = containerExecNotFound{
		exitCode: 1,
		line: func(id string) string {
			return `(?:Error response from daemon: |Error: )?No such container: ` + id
		},
	}
	// podman exec: libpod's define.ErrNoSuchCtr ("no such container"),
	// wrapped by the name/ID lookup or, if the container went away
	// mid-exec, the database lookup. Podman exits 125 for its own errors
	// (define.ExecErrorCodeGeneric), never for the command's.
	podmanExecNotFound = containerExecNotFound{
		exitCode: 125,
		line: func(id string) string {
			return `Error: no container with (?:name or ID "` + id + `" found|ID [0-9a-f]+ found in database): no such container`
		},
	}
	// Apple `container exec`: ContainerizationError(.notFound) from the
	// client's get ("get failed: container <id> not found") or, if the
	// container went away after it, the API server ("container with ID
	// <id> not found"); swift-argument-parser prints it as
	// `Error: notFound: "<message>"` and exits 1.
	appleExecNotFound = containerExecNotFound{
		exitCode: 1,
		line: func(id string) string {
			return `Error: notFound: "(?:get failed: container ` + id + `|container with ID ` + id + `) not found"`
		},
	}
)

// classifyExecErr returns err unchanged unless it is the CLI reporting that
// the container id does not exist, in which case it returns an error
// wrapping ErrContainerNotFound (and not the CLI's exit error).
//
// A command that ran in the container and exited non-zero must never be
// reported as a missing container, whatever it printed (ptone/scion#3470).
// So all of these must hold: the CLI exited with its own code for the
// failure, an output line is exactly the CLI's wording for this id, and a
// fresh list no longer shows the container. Any doubt (list failure,
// cancelled ctx) keeps the original error.
func (n containerExecNotFound) classifyExecErr(ctx context.Context, err error, out, id string, list func(context.Context, map[string]string) ([]api.AgentInfo, error)) error {
	var exitErr *exec.ExitError
	if err == nil || id == "" || !errors.As(err, &exitErr) || exitErr.ExitCode() != n.exitCode {
		return err
	}
	re, reErr := regexp.Compile(`(?m)^` + n.line(regexp.QuoteMeta(id)) + `\r?$`)
	if reErr != nil || !re.MatchString(out) {
		return err
	}
	if ctx.Err() != nil {
		return err
	}
	agents, listErr := list(ctx, nil)
	if listErr != nil || findContainerAgent(agents, id) != nil {
		return err
	}
	return fmt.Errorf("agent '%s' %w, it may have exited and been removed", id, ErrContainerNotFound)
}
