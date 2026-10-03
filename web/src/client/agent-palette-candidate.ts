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

import { agentCandidateId, type PaletteCandidate } from './chat-palette-types.js';

/** The agent fields an Agents-group row reads. */
export interface AgentCandidateSource {
  id: string;
  name?: string;
  slug?: string;
  project?: string;
}

/**
 * Builds the Agents-group row for `agent`, with an `agent` target carrying
 * its ID.
 *
 * The label is the name, falling back to the slug, then the ID. The
 * secondary label is the project name, falling back to the slug unless the
 * slug is already the label. Name, slug and project are all searchable.
 *
 * `activityMs` is always 0: there is no conversation recency here, so
 * ranking falls back to match tier, then label, then ID — see
 * `utils/chat-palette-match.ts`.
 */
export function buildAgentCandidate(agent: AgentCandidateSource): PaletteCandidate {
  const displayName = agent.name || agent.slug || agent.id;
  const slugHint = agent.slug && agent.slug !== displayName ? agent.slug : '';
  const secondaryLabel = agent.project || slugHint;
  const searchFields = [
    ...new Set([displayName, agent.slug, agent.project].filter((f): f is string => !!f)),
  ];
  return {
    id: agentCandidateId(agent.id),
    group: 'agents',
    label: displayName,
    secondaryLabel,
    searchFields,
    activityMs: 0,
    target: { kind: 'agent', agentId: agent.id, displayName },
  };
}
