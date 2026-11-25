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

import type { Agent } from '../types.js';
import { match } from './match.js';
import type { ValueSuggestionProvider } from './suggest.js';
import type { FilterRow, Query, Term } from './types.js';

export interface AgentFilterContext {
  currentUserId?: string | undefined;
  currentUserEmail?: string | undefined;
  /** Also match the project display name (`agent.project`), used by the agent graph. */
  includeProjectName?: boolean | undefined;
}

/**
 * Adapt an Agent row to the filterquery FilterRow interface.
 * Covers all agents, project_agents, and graph keys (phase, activity, label,
 * project, harness, broker, template, owner, mode) plus name/slug text search.
 */
export function agentToFilterRow(agent: Agent, ctx?: AgentFilterContext): FilterRow {
  return {
    values(key: string): string[] {
      switch (key.toLowerCase()) {
        case 'phase':
          return agent.phase ? [agent.phase] : [];
        case 'activity':
          return agent.activity ? [agent.activity] : [];
        case 'label': {
          if (!agent.labels) return [];
          const out: string[] = [];
          for (const [k, v] of Object.entries(agent.labels)) {
            out.push(k, `${k}=${v}`);
          }
          return out;
        }
        case 'project': {
          const out: string[] = [];
          if (agent.projectId) out.push(agent.projectId);
          if (agent.projectSlug) out.push(agent.projectSlug);
          if (ctx?.includeProjectName && agent.project) out.push(agent.project);
          return out;
        }
        case 'harness': {
          const h = agent.harnessConfig || agent.appliedConfig?.harnessConfig;
          return h ? [h] : [];
        }
        case 'broker': {
          const out: string[] = [];
          if (agent.runtimeBrokerId) out.push(agent.runtimeBrokerId);
          if (agent.runtimeBrokerName) out.push(agent.runtimeBrokerName);
          return out;
        }
        case 'template':
          return agent.template ? [agent.template] : [];
        case 'owner': {
          const out: string[] = [];
          if (agent.ownerId) {
            out.push(agent.ownerId);
            if (ctx?.currentUserId && agent.ownerId === ctx.currentUserId) {
              out.push('me');
              if (ctx.currentUserEmail) {
                out.push(ctx.currentUserEmail);
              }
            }
          }
          return out;
        }
        case 'mode': {
          const out: string[] = [agent.messageMode || 'project'];
          const canMsg = agent._messageability?.canMessage;
          if (canMsg === true) {
            out.push('can_message');
          } else if (canMsg === false) {
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

function isServerOnlyAgentTerm(t: Term, ctx?: AgentFilterContext): boolean {
  const k = t.key.toLowerCase();
  if (k === 'is') return true;
  if (k === 'owner') {
    const selfEmail = ctx?.currentUserEmail?.toLowerCase();
    return t.values.some((v) => v.includes('@') && (!selfEmail || v.toLowerCase() !== selfEmail));
  }
  return false;
}

/**
 * Returns true when `fq` contains any term that can only be resolved on the
 * server (`is:` project ownership classification, or `owner:<email>` for an
 * email other than the current user's). Live-add membership checks MUST be
 * conservative (refuse adds / mark stale) whenever this is true so the client
 * never admits rows the server would exclude.
 */
export function hasServerOnlyAgentTerms(fq: Query | null, ctx?: AgentFilterContext): boolean {
  if (!fq || fq.mode !== 'structured') return false;
  return fq.terms.some((t) => isServerOnlyAgentTerm(t, ctx));
}

/**
 * Strip server-resolved-only terms (`is:` project ownership classification,
 * and `owner:<email>` for emails other than the current user's) before local
 * evaluation over loaded Agent rows so client preview never falsely hides
 * server-matched rows.
 */
export function clientEvaluableAgentQuery(fq: Query, ctx?: AgentFilterContext): Query {
  if (fq.mode !== 'structured') return fq;
  const filteredTerms = fq.terms.filter((t) => !isServerOnlyAgentTerm(t, ctx));
  return { ...fq, terms: filteredTerms };
}

/** Evaluate a parsed agents filter query against an Agent row on the client. */
export function matchAgentQuery(fq: Query, agent: Agent, ctx?: AgentFilterContext): boolean {
  return match(clientEvaluableAgentQuery(fq, ctx), agentToFilterRow(agent, ctx));
}

/**
 * Conservative live-add membership check for SSE creates and drain-time adds:
 * returns false whenever `fq` contains any server-only term (`is:`, external-email
 * `owner:`) that the client cannot evaluate, and otherwise evaluates `fq` with
 * `phase` terms stripped (mirroring the hub's phase-independent stats and fit
 * population).
 */
export function canLiveAddAgent(fq: Query | null, agent: Agent, ctx?: AgentFilterContext): boolean {
  if (fq === null) return true;
  if (hasServerOnlyAgentTerms(fq, ctx)) return false;
  const nonPhase = nonPhasePredicateForQuery(fq, ctx);
  return nonPhase ? nonPhase(agent) : true;
}

/**
 * Build a non-phase predicate from `fq` (evaluating all terms except `phase`)
 * for the member-index population in paged mode, mirroring the hub's
 * phase-independent stats population (`statsFilter` with `phase` terms
 * removed). Returns `null` when `fq` has no non-phase terms.
 */
export function nonPhasePredicateForQuery(
  fq: Query | null,
  ctx?: AgentFilterContext
): ((agent: Agent) => boolean) | null {
  if (!fq) return null;
  if (fq.mode !== 'structured') {
    return (agent: Agent): boolean => matchAgentQuery(fq, agent, ctx);
  }
  const nonPhaseTerms = fq.terms.filter((t) => t.key.toLowerCase() !== 'phase');
  const hasText = Boolean(fq.text?.trim()) || Boolean(fq.excludeText && fq.excludeText.length > 0);
  if (nonPhaseTerms.length === 0 && !hasText) return null;
  const stripped: Query = { ...fq, terms: nonPhaseTerms };
  return (agent: Agent): boolean => matchAgentQuery(stripped, agent, ctx);
}

/**
 * Build a phase-only predicate from `fq` for off-page unknown SSE deltas that
 * carry only `phase` (ignoring other terms so they never falsely evict).
 */
export function phasePredicateForQuery(fq: Query | null): ((phase: string) => boolean) | null {
  if (!fq || fq.mode !== 'structured') return null;
  const phaseTerms = fq.terms.filter((t) => t.key.toLowerCase() === 'phase');
  if (phaseTerms.length === 0) return null;
  const phaseOnly: Query = { mode: 'structured', terms: phaseTerms };
  return (phase: string): boolean =>
    match(phaseOnly, {
      values: (key: string): string[] => (key === 'phase' ? [phase] : []),
      text: (): string[] => [],
    });
}

/** Returns true if `fq` contains any `is:` scope term (`is:mine` / `is:shared`). */
export function hasScopeTermInQuery(fq: Query | null): boolean {
  if (!fq || fq.mode !== 'structured') return false;
  return fq.terms.some((t) => t.key.toLowerCase() === 'is');
}

/**
 * Build a dynamic ValueSuggestionProvider backed by the currently loaded Agent
 * rows (design Q5). Enum keys (phase, activity, is, mode) are already supplied
 * by the schema; this fills in project, harness, broker, template, label, and
 * owner suggestions.
 */
export function buildAgentValueSuggestions(
  getAgents: () => readonly Agent[],
  getCtx?: () => AgentFilterContext
): ValueSuggestionProvider {
  return (key: string): string[] => {
    const agents = getAgents();
    const seen = new Set<string>();
    switch (key.toLowerCase()) {
      case 'project':
        for (const a of agents) {
          const val = a.projectSlug || a.projectId;
          if (val) seen.add(val);
        }
        break;
      case 'harness':
        for (const a of agents) {
          const val = a.harnessConfig || a.appliedConfig?.harnessConfig;
          if (val) seen.add(val);
        }
        break;
      case 'broker':
        for (const a of agents) {
          const val = a.runtimeBrokerName || a.runtimeBrokerId;
          if (val) seen.add(val);
        }
        break;
      case 'template':
        for (const a of agents) {
          if (a.template) seen.add(a.template);
        }
        break;
      case 'label':
        for (const a of agents) {
          if (!a.labels) continue;
          for (const [k, v] of Object.entries(a.labels)) {
            seen.add(k);
            seen.add(`${k}=${v}`);
          }
        }
        break;
      case 'owner': {
        const ctx = getCtx?.();
        if (ctx?.currentUserId) {
          seen.add('me');
        }
        for (const a of agents) {
          if (a.ownerId) seen.add(a.ownerId);
        }
        break;
      }
      default:
        break;
    }
    return [...seen];
  };
}
