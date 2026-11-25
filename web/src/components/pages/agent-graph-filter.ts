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

import type { Agent, Project } from '../../shared/types.js';
import { parentIdOf } from '../../shared/lineage.js';
import {
  agentToFilterRow,
  buildAgentValueSuggestions,
  match,
  type FilterRow,
  type Query,
  type ValueSuggestionProvider,
} from '../../shared/filter-query/index.js';

export interface GraphFilterResult {
  /** Matched agents union their ancestor chains, in original order. */
  agents: Agent[];
  /** Ancestors included only as lineage context (ancestors(M) \ M). */
  contextIds: ReadonlySet<string>;
  /** True when the query imposes at least one constraint. */
  hasActiveFilter: boolean;
}

const EMPTY_SET: ReadonlySet<string> = new Set();

/**
 * Reports whether q imposes no filter constraints.
 */
export function isEmptyGraphQuery(q: Query | null): boolean {
  if (!q) return true;
  if (q.mode === 'text') return q.text === '';
  return q.terms.length === 0 && !q.text && (!q.excludeText || q.excludeText.length === 0);
}

/**
 * Maps an Agent (compact or full) to a FilterRow for the graph schema.
 * Delegates to the shared `agentToFilterRow` accessor with project display-name
 * matching enabled.
 */
export function agentToGraphFilterRow(agent: Agent): FilterRow {
  return agentToFilterRow(agent, { includeProjectName: true });
}

/**
 * Evaluates q over allAgents and returns M ∪ ancestors(M) plus
 * contextIds = ancestors(M) \ M, preserving lineage via parentIdOf.
 */
export function filterGraphAgents(
  allAgents: readonly Agent[],
  query: Query | null
): GraphFilterResult {
  if (isEmptyGraphQuery(query)) {
    return {
      agents: allAgents as Agent[],
      contextIds: EMPTY_SET,
      hasActiveFilter: false,
    };
  }

  const byId = new Map<string, Agent>();
  const matched = new Set<string>();
  for (const agent of allAgents) {
    byId.set(agent.id, agent);
    if (match(query as Query, agentToGraphFilterRow(agent))) {
      matched.add(agent.id);
    }
  }

  if (matched.size === 0) {
    return {
      agents: [],
      contextIds: EMPTY_SET,
      hasActiveFilter: true,
    };
  }

  const contextIds = new Set<string>();
  for (const id of matched) {
    const seen = new Set<string>([id]);
    let cur = byId.get(id);
    while (cur) {
      const pid = parentIdOf(cur);
      if (!pid || seen.has(pid)) break;
      seen.add(pid);
      const parent = byId.get(pid);
      if (!parent) break;
      if (!matched.has(pid)) {
        contextIds.add(pid);
      }
      cur = parent;
    }
  }

  const agents = allAgents.filter((a) => matched.has(a.id) || contextIds.has(a.id));
  return {
    agents,
    contextIds,
    hasActiveFilter: true,
  };
}

/**
 * Builds a ValueSuggestionProvider over the currently loaded graph agents.
 */
export function makeGraphValueSuggestions(
  getAgents: () => readonly Agent[]
): ValueSuggestionProvider {
  const base = buildAgentValueSuggestions(getAgents);
  return (key: string, prefix: string): string[] => {
    const k = key.toLowerCase();
    if (k !== 'project' && k !== 'label' && k !== 'template' && k !== 'harness') {
      return [];
    }
    return base(k, prefix).sort((a, b) => a.localeCompare(b));
  };
}

/**
 * Returns the single positive `project:` value when q scopes to exactly one
 * project (no negated project terms and no multi-value OR), or null otherwise.
 */
export function extractSingleProjectScope(q: Query | null): string | null {
  if (!q || q.mode !== 'structured') return null;
  let found: string | null = null;
  for (const term of q.terms) {
    if (term.key.toLowerCase() !== 'project') continue;
    if (term.negate || term.values.length !== 1) return null;
    if (found !== null && found !== term.values[0]) return null;
    found = term.values[0];
  }
  return found;
}

const SLUG_OR_ID_RE = /^[a-z0-9][a-z0-9_-]*$/;

/**
 * Resolves the single positive `project:` term of `query` (or an optional
 * fallback project ID) to a canonical load target:
 * - matches ID, slug, or display name (case-insensitively for slug/name)
 *   against `knownProjects` and `knownAgents` to return the canonical `projectId`;
 * - if no loaded project/agent matches yet, falls back to the raw value only
 *   when it is lowercase slug/ID-shaped (which the hub's `?projectId=` resolves
 *   server-side); otherwise returns `''` (unscoped) so display names never hit
 *   the hub's impossible-UUID empty response.
 */
export function resolveProjectLoadTarget(
  q: Query | null,
  knownAgents: readonly Agent[],
  knownProjects: readonly Project[],
  fallbackId = ''
): string {
  const raw = extractSingleProjectScope(q) ?? (fallbackId || null);
  if (!raw) return '';
  const lower = raw.toLowerCase();

  for (const p of knownProjects) {
    if (p.id === raw || p.slug?.toLowerCase() === lower || p.name.toLowerCase() === lower) {
      return p.id;
    }
  }
  for (const a of knownAgents) {
    if (
      a.projectId === raw ||
      a.projectSlug?.toLowerCase() === lower ||
      a.project?.toLowerCase() === lower
    ) {
      return a.projectId;
    }
  }

  return SLUG_OR_ID_RE.test(raw) ? raw : '';
}

/**
 * Formats a `project:<value>` term, quoting the value when required by the
 * filter query grammar.
 */
export function formatProjectFilterTerm(value: string): string {
  const needsQuote =
    value === '' ||
    /[\s,"\\]/.test(value) ||
    value.startsWith('-') ||
    value.startsWith('>') ||
    value.startsWith('<');
  if (!needsQuote) return `project:${value}`;
  const escaped = value.replace(/\\/g, '\\\\').replace(/"/g, '\\"');
  return `project:"${escaped}"`;
}

/**
 * Migrates a legacy `?project=<id>` URL parameter into `?q=project:<slug|id>`.
 * Uses the project's slug when it is already known in `knownProjects` or
 * `knownAgents`, falling back to the raw parameter value. If `?q=` is already
 * present alongside `?project=`, prepends the project term unless `?q=`
 * already contains a `project:` term.
 */
export function migrateProjectParamToQuery(
  params: URLSearchParams,
  knownAgents: readonly Agent[] = [],
  knownProjects: readonly Project[] = []
): {
  queryText: string;
  migrated: boolean;
  initialProject: string;
} {
  const legacyProject = (params.get('project') || '').trim();
  const existingQ = params.get('q') || '';
  if (!legacyProject) {
    return { queryText: existingQ, migrated: false, initialProject: '' };
  }
  let displayVal = legacyProject;
  for (const p of knownProjects) {
    if (p.id === legacyProject && p.slug) {
      displayVal = p.slug;
      break;
    }
  }
  if (displayVal === legacyProject) {
    for (const a of knownAgents) {
      if (a.projectId === legacyProject && a.projectSlug) {
        displayVal = a.projectSlug;
        break;
      }
    }
  }
  const term = formatProjectFilterTerm(displayVal);
  const trimmedQ = existingQ.trim();
  const hasProjectTerm = /(?:^|\s)-?project:/i.test(trimmedQ);
  const queryText = !trimmedQ ? term : hasProjectTerm ? trimmedQ : `${term} ${trimmedQ}`;
  return { queryText, migrated: true, initialProject: legacyProject };
}
