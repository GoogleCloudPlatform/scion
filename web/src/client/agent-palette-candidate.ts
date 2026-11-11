/**
 * Copyright 2026 Google LLC
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

/**
 * The Agents-group row shared by every "Jump to agent" palette: each surface
 * decides which agents are candidates, and this decides how one looks.
 *
 * Kept free of API and component imports so a surface can build candidates
 * from agents it already holds without pulling either into its bundle.
 */

import { agentCandidateId, type PaletteCandidate } from './palette-types.js';

/** The agent fields an Agents-group row reads. */
export interface AgentCandidateSource {
  id: string;
  name?: string;
  slug?: string;
  projectId?: string;
  /** The project's display name, as the hub resolves it on agent rows. */
  project?: string;
  /** The project's slug, as the hub resolves it on agent rows. */
  projectSlug?: string;
}

/**
 * Resolves a project ID to its slug, or `undefined` while the slug is not
 * known on this surface. A fallback for a row without `projectSlug`.
 */
export type ProjectSlugLookup = (projectId: string) => string | undefined;

/** The text of an Agents-group row: what it shows and what a query matches. */
export interface AgentRowText {
  label: string;
  secondaryLabel: string;
  searchFields: string[];
}

/**
 * The text of the Agents-group row for `agent`.
 *
 * The label is the name, falling back to the slug, then the ID. The
 * secondary label names the agent's project, since same-named agents in
 * different projects are otherwise indistinguishable: the row's project
 * slug, else the slug `projectSlug` knows for its project, else the project
 * name, else the agent slug unless the slug is already the label. Name,
 * agent slug, project slug and project name are all searchable.
 */
export function agentRowText(
  agent: AgentCandidateSource,
  projectSlug?: ProjectSlugLookup
): AgentRowText {
  const label = agent.name || agent.slug || agent.id;
  const slugHint = agent.slug && agent.slug !== label ? agent.slug : '';
  const knownProjectSlug =
    agent.projectSlug || (agent.projectId ? projectSlug?.(agent.projectId) || '' : '');
  const secondaryLabel = knownProjectSlug || agent.project || slugHint;
  const searchFields = [
    ...new Set(
      [label, agent.slug, knownProjectSlug, agent.project].filter((f): f is string => !!f)
    ),
  ];
  return { label, secondaryLabel, searchFields };
}

/**
 * Builds the Agents-group row for `agent`, with an `agent` target carrying
 * its ID. The row text is {@link agentRowText}.
 *
 * `activityMs` is always 0: there is no conversation recency here, so
 * ranking falls back to match tier, then label, then ID — see
 * `utils/palette-match.ts`.
 */
export function buildAgentCandidate(
  agent: AgentCandidateSource,
  projectSlug?: ProjectSlugLookup
): PaletteCandidate {
  const { label, secondaryLabel, searchFields } = agentRowText(agent, projectSlug);
  return {
    id: agentCandidateId(agent.id),
    group: 'agents',
    label,
    secondaryLabel,
    searchFields,
    activityMs: 0,
    target: { kind: 'agent', agentId: agent.id, displayName: label },
  };
}
