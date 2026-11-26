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
import { canonical, serverSubset } from './canonical.js';
import { match } from './match.js';
import type { ValueSuggestionProvider } from './suggest.js';
import type { FilterRow, Query, Term, ViewSchema } from './types.js';

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
 * Returns true when `fq` has any non-phase server-evaluable predicate (any
 * non-phase, non-client-only key term, or positive/excluded free text). When
 * false, the query only constrains `phase:` and/or client-only keys (`mode:`),
 * so the hub's phase-independent population is unfiltered.
 */
export function hasNonPhaseServerPredicate(fq: Query | null, schema: ViewSchema): boolean {
  if (!fq) return false;
  if (fq.mode !== 'structured') {
    return Boolean(fq.text?.trim());
  }
  if (fq.text?.trim() || (fq.excludeText && fq.excludeText.length > 0)) {
    return true;
  }
  return fq.terms.some((t) => {
    const k = t.key.toLowerCase();
    if (k === 'phase') return false;
    const spec = schema.keys.find((ks) => ks.name.toLowerCase() === k);
    return spec ? spec.exec !== 'client' : false;
  });
}

/**
 * Build a client-side predicate matching the hub's phase-independent stats
 * population: all non-phase, non-client-only terms plus positive/excluded free
 * text (server-only terms such as `is:` are stripped by `matchAgentQuery`
 * because the server already applied them). Used to compute local `stats` and
 * Stop-all counts over `H` when a drain could not push down a text- or
 * exclusion-only remainder.
 */
export function nonPhaseServerPredicateForQuery(
  fq: Query | null,
  schema: ViewSchema,
  ctx?: AgentFilterContext
): ((agent: Agent) => boolean) | null {
  if (!fq) return null;
  if (fq.mode !== 'structured') {
    return fq.text?.trim() ? (agent: Agent): boolean => matchAgentQuery(fq, agent, ctx) : null;
  }
  const serverNonPhaseTerms = fq.terms.filter((t) => {
    const k = t.key.toLowerCase();
    if (k === 'phase') return false;
    const spec = schema.keys.find((ks) => ks.name.toLowerCase() === k);
    return spec ? spec.exec !== 'client' : false;
  });
  const hasText = Boolean(fq.text?.trim()) || Boolean(fq.excludeText && fq.excludeText.length > 0);
  if (serverNonPhaseTerms.length === 0 && !hasText) return null;
  const stripped: Query = { ...fq, terms: serverNonPhaseTerms };
  return (agent: Agent): boolean => matchAgentQuery(stripped, agent, ctx);
}

/**
 * Deterministic key for the phase-independent server population of `fq` (used
 * to key project 422 refusal memory and fit-loaded population identity). Unlike
 * `drainServerQuery`, this never collapses unrepresentable text/exclusion-only
 * remainders (such as `phase:running -test`) onto the unfiltered `''` key.
 */
export function refusalKeyForQuery(fq: Query | null, schema: ViewSchema): string {
  if (!fq) return '';
  if (fq.mode !== 'structured') {
    return canonical(fq);
  }
  const serverNonPhaseTerms = fq.terms.filter((t) => {
    const k = t.key.toLowerCase();
    if (k === 'phase') return false;
    const spec = schema.keys.find((ks) => ks.name.toLowerCase() === k);
    return spec ? spec.exec !== 'client' : false;
  });
  if (
    serverNonPhaseTerms.length === 0 &&
    !fq.text?.trim() &&
    (!fq.excludeText || fq.excludeText.length === 0)
  ) {
    return '';
  }
  return canonical({
    mode: 'structured',
    terms: serverNonPhaseTerms,
    ...(fq.text ? { text: fq.text } : {}),
    ...(fq.excludeText?.length ? { excludeText: fq.excludeText } : {}),
  });
}

/**
 * Canonical server `?q=` string for a complete-set drain (or complete fit
 * `loadedServerQ`): strips `phase` terms before projecting to `serverSubset`,
 * so drained held sets are phase-independent (matching fit and paged stats
 * populations) and local rendering applies `phase:` on top.
 */
export function drainServerQuery(fq: Query | null, schema: ViewSchema): string {
  if (!fq) return '';
  if (fq.mode !== 'structured') return serverSubset(fq, schema);
  const stripped: Query = {
    ...fq,
    terms: fq.terms.filter((t) => t.key.toLowerCase() !== 'phase'),
  };
  return serverSubset(stripped, schema);
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
