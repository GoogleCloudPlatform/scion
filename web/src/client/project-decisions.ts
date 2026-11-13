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
 * Project Decisions API client & types (ptone/scion#7800, Plan §5.1–§5.2).
 *
 * Mirrors `pkg/hub/choices_project.go` (`GET /api/v1/projects/{id}/decisions`).
 */

import { apiFetch, extractApiError } from './api.js';
import type { ChoiceAnswerMode, DecisionView } from '../shared/types.js';

/** Live pending counts across conversations in the project the caller can read. */
export interface ProjectDecisionCounts {
  waitingOnYou: number;
  waitingOnOthers: number;
}

/** One filter-facet option (an asking agent or a recorded answerer) scoped to readable conversations. */
export interface ProjectDecisionActorView {
  id: string;
  name: string;
}

/** Per-row reason when a non-settled decision row is in "Waiting on others". */
export type ProjectDecisionWaitingReason =
  | 'read_only'
  | 'message_denied'
  | 'other_user_confirm'
  | (string & {});

/** One prompt card on `tab=pending`. */
export interface ProjectDecisionPromptView {
  promptMessageId: string;
  promptAvailable: boolean;
  chatKey: string;
  conversationLabel: string;
  conversationAvailable: boolean;
  askerAgentId: string;
  askerAgentName: string;
  createdAt: string;
  answerMode: ChoiceAnswerMode | (string & {});
  waitingOnYou: boolean;
  canAnswer: boolean;
  disabledReason?: string;
  waitingReasons?: Record<string, ProjectDecisionWaitingReason>;
  decisions: DecisionView[];
}

/** Decision metadata embedded in one entry of the `tab=made` decision log. */
export interface ProjectDecisionSummaryView {
  rowId: string;
  promptMessageId: string;
  promptAvailable: boolean;
  key: string;
  ref: string;
  version: number;
  question: string;
  askerAgentId: string;
  askerAgentName: string;
}

/** One append-only answer log line on `tab=made`. */
export interface ProjectDecisionLogEntryView {
  answerId: string;
  decision: ProjectDecisionSummaryView;
  choiceId: string;
  label: string;
  matchedDefault: boolean;
  previousChoiceLabel?: string;
  answeredByUserId?: string;
  answeredByName: string;
  provenance: string;
  outcome: string;
  surface: string;
  interpretedByAgentId?: string;
  interpretedByAgentName?: string;
  sourceMessageId?: string;
  sourceMessageAvailable: boolean;
  responseMessageId?: string;
  batchId?: string;
  at: string;
  chatKey: string;
  conversationLabel: string;
  conversationAvailable: boolean;
}

/** Response of `GET /api/v1/projects/{id}/decisions?tab=pending`. */
export interface ProjectPendingDecisionsResponse {
  counts: ProjectDecisionCounts;
  prompts: ProjectDecisionPromptView[];
  agents: ProjectDecisionActorView[];
  answerers: ProjectDecisionActorView[];
  nextCursor?: string;
}

/** Response of `GET /api/v1/projects/{id}/decisions?tab=made`. */
export interface ProjectMadeDecisionsResponse {
  counts: ProjectDecisionCounts;
  entries: ProjectDecisionLogEntryView[];
  agents: ProjectDecisionActorView[];
  answerers: ProjectDecisionActorView[];
  nextCursor?: string;
}

/** Provenance filter values accepted by `tab=made`. */
export type MadeDecisionsProvenanceFilter =
  | ''
  | 'direct'
  | 'interpreted'
  | 'confirmed'
  | 'overridden';

/** Combinable filter criteria for `tab=made`. */
export interface MadeDecisionsFilter {
  agent?: string;
  user?: string;
  provenance?: MadeDecisionsProvenanceFilter | (string & {});
  from?: string;
  to?: string;
}

export interface ProjectDecisionsQueryOptions {
  tab?: 'pending' | 'made' | undefined;
  cursor?: string | undefined;
  limit?: number | undefined;
  filter?: MadeDecisionsFilter | undefined;
}

function cleanFilterValue(raw: string | undefined): string {
  const trimmed = (raw ?? '').trim();
  if (!trimmed || trimmed === 'all') return '';
  return trimmed;
}

/**
 * Builds the query string (starting with `?`) for
 * `GET /api/v1/projects/{id}/decisions`, omitting empty/`'all'` filters.
 */
export function buildProjectDecisionsQuery(options: ProjectDecisionsQueryOptions = {}): string {
  const params = new URLSearchParams();
  const tab = options.tab ?? 'pending';
  params.set('tab', tab);

  if (options.limit !== undefined) {
    params.set('limit', String(options.limit));
  }
  const cursor = (options.cursor ?? '').trim();
  if (cursor) {
    params.set('cursor', cursor);
  }

  if (tab === 'made' && options.filter) {
    const agent = cleanFilterValue(options.filter.agent);
    if (agent) params.set('agent', agent);

    const user = cleanFilterValue(options.filter.user);
    if (user) params.set('user', user);

    const provenance = cleanFilterValue(options.filter.provenance);
    if (provenance) params.set('provenance', provenance);

    const from = cleanFilterValue(options.filter.from);
    if (from) params.set('from', from);

    const to = cleanFilterValue(options.filter.to);
    if (to) params.set('to', to);
  }

  return `?${params.toString()}`;
}

function projectDecisionsUrl(projectId: string, options: ProjectDecisionsQueryOptions): string {
  return `/api/v1/projects/${encodeURIComponent(projectId)}/decisions${buildProjectDecisionsQuery(options)}`;
}

/** Fetches `GET /api/v1/projects/{id}/decisions?tab=pending`. */
export async function fetchProjectPendingDecisions(
  projectId: string,
  options: { cursor?: string | undefined; limit?: number | undefined; signal?: AbortSignal | undefined } = {}
): Promise<ProjectPendingDecisionsResponse> {
  const url = projectDecisionsUrl(projectId, {
    tab: 'pending',
    cursor: options.cursor,
    limit: options.limit,
  });
  const res = await apiFetch(url, options.signal ? { signal: options.signal } : undefined);
  if (!res.ok) {
    throw new Error(await extractApiError(res, `HTTP ${res.status}`));
  }
  const data = (await res.json()) as Partial<ProjectPendingDecisionsResponse> | null;
  return {
    counts: {
      waitingOnYou: data?.counts?.waitingOnYou ?? 0,
      waitingOnOthers: data?.counts?.waitingOnOthers ?? 0,
    },
    prompts: Array.isArray(data?.prompts) ? data.prompts : [],
    agents: Array.isArray(data?.agents) ? data.agents : [],
    answerers: Array.isArray(data?.answerers) ? data.answerers : [],
    ...(data?.nextCursor ? { nextCursor: data.nextCursor } : {}),
  };
}

/** Fetches `GET /api/v1/projects/{id}/decisions?tab=made`. */
export async function fetchProjectMadeDecisions(
  projectId: string,
  filter: MadeDecisionsFilter = {},
  options: { cursor?: string | undefined; limit?: number | undefined; signal?: AbortSignal | undefined } = {}
): Promise<ProjectMadeDecisionsResponse> {
  const url = projectDecisionsUrl(projectId, {
    tab: 'made',
    cursor: options.cursor,
    limit: options.limit,
    filter,
  });
  const res = await apiFetch(url, options.signal ? { signal: options.signal } : undefined);
  if (!res.ok) {
    throw new Error(await extractApiError(res, `HTTP ${res.status}`));
  }
  const data = (await res.json()) as Partial<ProjectMadeDecisionsResponse> | null;
  return {
    counts: {
      waitingOnYou: data?.counts?.waitingOnYou ?? 0,
      waitingOnOthers: data?.counts?.waitingOnOthers ?? 0,
    },
    entries: Array.isArray(data?.entries) ? data.entries : [],
    agents: Array.isArray(data?.agents) ? data.agents : [],
    answerers: Array.isArray(data?.answerers) ? data.answerers : [],
    ...(data?.nextCursor ? { nextCursor: data.nextCursor } : {}),
  };
}

/**
 * Fetches live pending counts (`tab=pending&limit=0`) for the project header
 * badge without loading prompt bodies or facets.
 */
export async function fetchProjectDecisionCounts(
  projectId: string,
  signal?: AbortSignal
): Promise<ProjectDecisionCounts> {
  const res = await fetchProjectPendingDecisions(projectId, { limit: 0, signal });
  return res.counts;
}
