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

import type { Agent } from '../../shared/types.js';
import { parentIdOf } from '../../shared/lineage.js';
import {
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
 * Note: small duplication with agents-list field accessors (to be unified after
 * parallel P2/P3 landing).
 */
export function agentToGraphFilterRow(agent: Agent): FilterRow {
  return {
    values(key: string): string[] {
      switch (key.toLowerCase()) {
        case 'phase':
          return agent.phase ? [agent.phase] : [];
        case 'activity':
          return agent.activity ? [agent.activity] : [];
        case 'label': {
          const out: string[] = [];
          if (agent.labels) {
            for (const [k, v] of Object.entries(agent.labels)) {
              out.push(k, `${k}=${v}`);
            }
          }
          return out;
        }
        case 'project': {
          const out: string[] = [];
          if (agent.projectSlug) out.push(agent.projectSlug);
          if (agent.projectId) out.push(agent.projectId);
          if (agent.project) out.push(agent.project);
          return out;
        }
        case 'template':
          return agent.template ? [agent.template] : [];
        case 'harness': {
          // harnessVendor is a vendor, not a harness config.
          const hc = agent.harnessConfig || agent.appliedConfig?.harnessConfig;
          return hc ? [hc] : [];
        }
        case 'mode': {
          const out: string[] = [agent.messageMode || 'project'];
          if (agent._messageability?.canMessage === true) {
            out.push('can_message');
          } else if (agent._messageability?.canMessage === false) {
            out.push('cannot_message');
          }
          return out;
        }
        default:
          return [];
      }
    },
    text(): string[] {
      const out: string[] = [];
      if (agent.name) out.push(agent.name);
      if (agent.slug) out.push(agent.slug);
      return out;
    },
  };
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
      agents: [...allAgents],
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
  return (key: string): string[] => {
    const agents = getAgents();
    const set = new Set<string>();
    switch (key.toLowerCase()) {
      case 'project':
        for (const a of agents) {
          const val = a.projectSlug || a.projectId;
          if (val) set.add(val);
        }
        break;
      case 'label':
        for (const a of agents) {
          if (!a.labels) continue;
          for (const [k, v] of Object.entries(a.labels)) {
            set.add(k);
            set.add(`${k}=${v}`);
          }
        }
        break;
      case 'template':
        for (const a of agents) {
          if (a.template) set.add(a.template);
        }
        break;
      case 'harness':
        for (const a of agents) {
          const hc = a.harnessConfig || a.appliedConfig?.harnessConfig;
          if (hc) set.add(hc);
        }
        break;
      default:
        return [];
    }
    return Array.from(set).sort((a, b) => a.localeCompare(b));
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
 * Migrates a legacy `?project=<id>` URL parameter into `?q=project:<id>`.
 * If `?q=` is already present alongside `?project=`, prepends the project term
 * unless `?q=` already contains a `project:` term.
 */
export function migrateProjectParamToQuery(params: URLSearchParams): {
  queryText: string;
  migrated: boolean;
  initialProject: string;
} {
  const legacyProject = (params.get('project') || '').trim();
  const existingQ = params.get('q') || '';
  if (!legacyProject) {
    return { queryText: existingQ, migrated: false, initialProject: '' };
  }
  const term = formatProjectFilterTerm(legacyProject);
  const trimmedQ = existingQ.trim();
  const hasProjectTerm = /(?:^|\s)-?project:/i.test(trimmedQ);
  const queryText = !trimmedQ ? term : hasProjectTerm ? trimmedQ : `${term} ${trimmedQ}`;
  return { queryText, migrated: true, initialProject: legacyProject };
}
