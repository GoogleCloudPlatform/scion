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

package runtimebroker

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// previousRuntimeAmbiguousError is previousRuntimeMatch's answer when more
// than one container of the agent was found in the other registered
// runtimes: none of them is taken as the agent's, so nothing is stopped.
type previousRuntimeAmbiguousError struct {
	id         string
	containers int
}

func (e *previousRuntimeAmbiguousError) Error() string {
	return fmt.Sprintf("agent '%s' is ambiguous: %d containers match in other runtimes", e.id, e.containers)
}

// previousRuntimeMatch searches the registered runtimes other than the
// agent's own one (own) for a container of agent id in projectID. Callers
// use it only when the own runtime holds no container for the agent
// (prior, priorErr: that lookup's result, an ErrAgentNotFound). The
// runtime the agent's profile selects can differ from the one its
// container runs in when the profile's runtime, context or namespace was
// changed (ptone/scion#4722), and the container left there must not keep
// running beside a new one.
//
// The search is delete's (collectAgentCandidates, with its project and
// legacy-path checks, so a same-named agent of another project is never
// matched). Each runtime is listed separately: one that cannot be listed is
// logged with logMsg and skipped, and the others are still searched. The
// result:
//   - one container: the match in that runtime (otherRuntimeMatch);
//   - more than one: a *previousRuntimeAmbiguousError, so no guess is
//     stopped;
//   - none: prior and priorErr, except that when prior is unmatched and
//     the search found exactly one entry for the agent's files, that entry
//     is returned (still with a not-found error) so a caller can read the
//     agent's project path from it.
func (s *Server) previousRuntimeMatch(ctx context.Context, own *agentOwnRuntime, id, projectID string, prior agentMatch, priorErr error, logMsg string) (agentMatch, error) {
	cands, _ := s.collectAgentCandidates(ctx, s.otherManagers(ctx, own), id, projectID, logMsg)
	var containers []agentCandidate
	for _, c := range cands {
		if c.entry.ContainerID != "" {
			containers = append(containers, c)
		}
	}
	switch len(containers) {
	case 0:
		if !prior.matched && len(cands) == 1 {
			c := cands[0]
			// Paired through runtimeOfManager, so no record: the zero
			// value.
			return agentMatch{manager: c.mgr, runtime: s.runtimeOfManager(c.mgr), entry: c.entry, matched: true},
				&agentNotFoundError{slug: strings.ToLower(id)}
		}
	case 1:
		if m, err := s.otherRuntimeMatch(strings.ToLower(id), containers[0]); err == nil {
			return m, nil
		}
	default:
		return agentMatch{}, &previousRuntimeAmbiguousError{id: id, containers: len(containers)}
	}
	return prior, priorErr
}

// previousRuntimeRefusal is stopInPreviousRuntime's reason to refuse a
// start: msg is the response text, which names the agent and, for a failed
// stop, the runtime; it never carries the runtime's raw error, which is
// logged.
type previousRuntimeRefusal struct {
	msg string
}

func (e *previousRuntimeRefusal) Error() string { return e.msg }

// stopInPreviousRuntime runs before a start of agent id in its own runtime
// (own, the runtime the start runs in). When own holds no container for the
// agent, a container of it in another registered runtime
// (previousRuntimeMatch) is stopped first, as restart stops it, so the
// agent never runs in two runtimes at once (ptone/scion#4722). It returns a
// *previousRuntimeRefusal when the start must not go ahead: that container
// could not be stopped, or more than one was found. A stop answering that
// the container is already gone or not running is not a refusal.
//
// Anything else leaves the start as it was: own lists the agent (the
// runtime's own start replaces it), own cannot be listed (the start reports
// that), or no other runtime holds a container.
func (s *Server) stopInPreviousRuntime(ctx context.Context, own *agentOwnRuntime, id, projectID string) error {
	slug := strings.ToLower(id)
	agents, err := listInOwnRuntime(ctx, own, slug, projectID)
	if err != nil {
		return nil
	}
	prior, priorErr := agentMatchFrom(slug, agents, own.mgr, own.rt, own.record)
	if !errors.Is(priorErr, ErrAgentNotFound) {
		return nil
	}
	m, err := s.previousRuntimeMatch(ctx, own, id, projectID, prior, priorErr,
		"Start: runtime list failed while searching the other runtimes; skipped")
	var ambiguous *previousRuntimeAmbiguousError
	if errors.As(err, &ambiguous) {
		s.agentLifecycleLog.Warn("Start refused: more than one container of the agent found in other runtimes",
			"agent_id", id, "project_id", projectID, "containers", ambiguous.containers)
		return &previousRuntimeRefusal{msg: fmt.Sprintf(
			"agent '%s' was not started: %d containers of it were found in other runtimes; stop them first", id, ambiguous.containers)}
	}
	if err != nil || m.containerID == "" || m.manager == nil {
		return nil
	}
	runtimeName := "unknown"
	if m.runtime != nil {
		runtimeName = m.runtime.Name()
	}
	// The stop target is the operation ID, namespace-qualified for a
	// Kubernetes pod, so it also names where the container was found.
	ref := resolvedStopRef(m.containerID, m, "")
	if err := m.manager.StopTarget(ctx, ref); err != nil {
		if isContainerStopTolerable(err) {
			s.agentLifecycleLog.Warn("Start: container left in previous runtime already gone or stopped",
				"agent_id", id, "project_id", projectID, "runtime", runtimeName,
				"target", ref.ID, "error", err)
			return nil
		}
		s.agentLifecycleLog.Error("Start refused: container left in previous runtime could not be stopped",
			"agent_id", id, "project_id", projectID, "runtime", runtimeName,
			"target", ref.ID, "error", err)
		return &previousRuntimeRefusal{msg: fmt.Sprintf(
			"agent '%s' was not started: its container in previous runtime '%s' could not be stopped", id, runtimeName)}
	}
	s.agentLifecycleLog.Info("Start: stopped container left in previous runtime",
		"agent_id", id, "project_id", projectID, "runtime", runtimeName, "target", ref.ID)
	return nil
}
