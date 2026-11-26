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
 * Mirrors `pkg/hub/decisions_project.go` (`GET /api/v1/projects/{id}/decisions`).
 */

import { apiFetch, parseApiError } from './api.js';
import type {
  AnswerView,
  ChoiceOption,
  DecisionAnswerMode,
  DecisionView,
  OtherOption,
} from '../shared/types.js';

/** Error code returned by the Hub when bounded decisions search times out (D9). */
export const DECISIONS_SEARCH_TIMEOUT_CODE = 'SEARCH_TIMEOUT';

/** Minimum and maximum pure-text rune counts accepted for `q` by `pkg/hub/decisions_filter_query.go`. */
export const MIN_DECISIONS_SEARCH_RUNES = 2;
export const MAX_DECISIONS_TEXT_RUNES = 200;
/** Maximum UTF-8 byte length accepted for filter queries (`filterquery.MaxInputBytes`). */
export const MAX_DECISIONS_FILTER_QUERY_BYTES = 512;

/** Maximum UTF-8 byte length accepted for `conversation` by `pkg/hub/decisions_project.go`. */
export const MAX_DECISIONS_CONVERSATION_BYTES = 256;

const AGENT_UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const CALENDAR_DATE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;
// Matches Go strings.TrimSpace (which trims U+0085 NEL) plus JS whitespace/U+FEFF BOM at string edges.
const EDGE_SPACE_RE = /^[\s\u0085]+|[\s\u0085]+$/g;
// eslint-disable-next-line no-control-regex -- reject C0/DEL/C1 control characters in URL parameters
const CONTROL_CHAR_RE = /[\u0000-\u001f\u007f-\u009f]/;

/** Live pending counts across conversations in the project the caller can read. */
export interface ProjectDecisionCounts {
  waitingOnYou: number;
  waitingOnOthers: number;
}

/** One filter-facet option (an asking agent or a recorded answerer) scoped to readable conversations. */
export interface ProjectDecisionActorView {
  id: string;
  name: string;
  hint?: string;
  projectId?: string;
}

/** One conversation filter-facet option scoped to readable conversations with at least one decision (D5). */
export interface ProjectDecisionConversationView {
  key: string;
  label: string;
  projectId: string;
  hint?: string;
}

/** Project summary carried on each decision prompt or log entry (S2). */
export interface ProjectDecisionProjectRef {
  id: string;
  name: string;
  slug: string;
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
  project?: ProjectDecisionProjectRef;
  askerAgentId: string;
  askerAgentName: string;
  createdAt: string;
  answerMode: DecisionAnswerMode | (string & {});
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
  /** decisions.status: "pending", "decided", "replaced", "withdrawn", or "dismissed". */
  status?: string;
  question: string;
  /** Every offered choice, in canonical order (ptone/scion#8443). */
  choices?: ChoiceOption[];
  /** The recommended choice; absent when the agent recommended none. */
  defaultChoiceId?: string;
  other?: OtherOption;
  /** The version that replaced this one (`--update`). */
  replacedBy?: ProjectDecisionRef;
  askerAgentId: string;
  askerAgentName: string;
}

/** A decision version named by row ID and version number (DecisionRef). */
export interface ProjectDecisionRef {
  rowId: string;
  version: number;
}

/** One Made entry: a decision version's final answer (D2 in ptone/scion#8443). */
export interface ProjectDecisionLogEntryView {
  answerId: string;
  decision: ProjectDecisionSummaryView;
  choiceId: string;
  label: string;
  otherText?: string;
  matchedDefault: boolean;
  previousChoiceLabel?: string;
  previousOtherText?: string;
  /** The superseded agent-interpreted answer folded into this direct entry. */
  interpretedAnswer?: AnswerView;
  /** An interpreted entry with no direct answer yet, on a non-terminal decision. */
  awaitingConfirmation?: boolean;
  answeredByUserId?: string;
  answeredByName: string;
  provenance: string;
  outcome: string;
  dismissReason?: string;
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
  project?: ProjectDecisionProjectRef;
}

/** Response of `GET /api/v1/projects/{id}/decisions?tab=pending`. */
export interface ProjectPendingDecisionsResponse {
  counts: ProjectDecisionCounts;
  filteredCounts?: ProjectDecisionCounts;
  prompts: ProjectDecisionPromptView[];
  agents: ProjectDecisionActorView[];
  answerers: ProjectDecisionActorView[];
  conversations?: ProjectDecisionConversationView[];
  nextCursor?: string;
}

/** Response of `GET /api/v1/projects/{id}/decisions?tab=made`. */
export interface ProjectMadeDecisionsResponse {
  counts: ProjectDecisionCounts;
  entries: ProjectDecisionLogEntryView[];
  agents: ProjectDecisionActorView[];
  answerers: ProjectDecisionActorView[];
  conversations?: ProjectDecisionConversationView[];
  nextCursor?: string;
}

/** Provenance filter values accepted by `tab=made`. */
export type MadeDecisionsProvenanceFilter =
  | ''
  | 'direct'
  | 'interpreted'
  | 'confirmed'
  | 'overridden'
  | 'dismissed';

/** Shared filter criteria accepted by both `tab=pending` and `tab=made` (D6, D7). */
export interface SharedDecisionsFilter {
  q?: string;
  conversation?: string;
  agent?: string;
}

/** Combinable filter criteria for `tab=made` (shared filters plus Made-only filters). */
export interface MadeDecisionsFilter extends SharedDecisionsFilter {
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
  validateQuery?: ((q: string) => boolean) | undefined;
}

/** Validated URL filter/tab state for the project Decisions page (D10, D11). */
export interface DecisionsUrlState {
  tab: 'pending' | 'made';
  q: string;
  conversation: string;
  agent: string;
  user: string;
  provenance: MadeDecisionsProvenanceFilter;
  from: string;
  to: string;
}

/** Structured error thrown by project Decisions API helpers, carrying HTTP status and Hub error code. */
export class DecisionsApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(message: string, status: number, code = '') {
    super(message);
    this.name = 'DecisionsApiError';
    this.status = status;
    this.code = code;
  }

  get isSearchTimeout(): boolean {
    return this.code === DECISIONS_SEARCH_TIMEOUT_CODE;
  }
}

export function isDecisionsSearchTimeoutError(err: unknown): boolean {
  return err instanceof DecisionsApiError && err.isSearchTimeout;
}

/**
 * Reports whether the Hub response includes the `conversations` facet (section 5.3 / 7),
 * indicating that the Hub supports P1+ search and conversation/pending filters.
 */
export function hasDecisionsSearchCapability(
  res: { conversations?: ProjectDecisionConversationView[] | undefined } | null | undefined
): boolean {
  return Array.isArray(res?.conversations);
}

function trimEdgeSpace(raw: string | undefined | null): string {
  return (raw ?? '').replace(EDGE_SPACE_RE, '');
}

/**
 * Normalizes a `q` parameter for the Decisions filter query dialect: trims
 * edge whitespace, rejects control characters, enforces the 512-byte
 * `filterquery.MaxInputBytes` bound, and runs an optional caller-supplied
 * `validateQuery` predicate (kept injected so this module does not pull
 * `shared/filter-query` into profile-shell/profile-nav closures).
 */
export function normalizeDecisionsFilterQuery(
  raw: string | undefined | null,
  validateQuery?: (q: string) => boolean
): string {
  const trimmed = trimEdgeSpace(raw);
  if (!trimmed || CONTROL_CHAR_RE.test(trimmed)) return '';
  const runeCount = [...trimmed].length;
  if (runeCount < MIN_DECISIONS_SEARCH_RUNES) return '';
  if (new TextEncoder().encode(trimmed).length > MAX_DECISIONS_FILTER_QUERY_BYTES) {
    return '';
  }
  if (validateQuery) {
    return validateQuery(trimmed) ? trimmed : '';
  }
  if (!trimmed.includes(':') && runeCount > MAX_DECISIONS_TEXT_RUNES) {
    return '';
  }
  return trimmed;
}

/**
 * Returns the trimmed raw `q` query parameter (without control chars) for
 * displaying an invalid URL query in `<scion-filter-box>` with its inline error
 * while loading the unfiltered list.
 */
export function extractRawDecisionsQueryParam(search: string | URLSearchParams): string {
  const params = typeof search === 'string' ? new URLSearchParams(search) : search;
  const trimmed = trimEdgeSpace(params.get('q'));
  if (!trimmed || CONTROL_CHAR_RE.test(trimmed)) return '';
  return trimmed;
}

export function isValidDecisionAgentId(raw: string | undefined | null): boolean {
  const trimmed = trimEdgeSpace(raw);
  return trimmed.length > 0 && AGENT_UUID_RE.test(trimmed);
}

export function isValidDecisionCalendarDate(raw: string | undefined | null): boolean {
  const trimmed = trimEdgeSpace(raw);
  const match = CALENDAR_DATE_RE.exec(trimmed);
  if (!match) return false;
  const year = Number(match[1]);
  const month = Number(match[2]);
  const day = Number(match[3]);
  if (month < 1 || month > 12 || day < 1 || day > 31) return false;
  const dt = new Date(Date.UTC(year, month - 1, day));
  return dt.getUTCFullYear() === year && dt.getUTCMonth() === month - 1 && dt.getUTCDate() === day;
}

function cleanFilterValue(raw: string | undefined | null): string {
  const trimmed = trimEdgeSpace(raw);
  if (!trimmed || trimmed === 'all') return '';
  return trimmed;
}

function cleanConversationValue(raw: string | undefined | null): string {
  const trimmed = cleanFilterValue(raw);
  if (!trimmed || CONTROL_CHAR_RE.test(trimmed)) return '';
  if (new TextEncoder().encode(trimmed).length > MAX_DECISIONS_CONVERSATION_BYTES) {
    return '';
  }
  return trimmed;
}

function cleanUserValue(raw: string | undefined | null): string {
  const trimmed = cleanFilterValue(raw);
  if (!trimmed || trimmed.length > 256 || CONTROL_CHAR_RE.test(trimmed)) return '';
  return trimmed;
}

function cleanProvenanceValue(raw: string | undefined | null): MadeDecisionsProvenanceFilter {
  const trimmed = cleanFilterValue(raw);
  switch (trimmed) {
    case 'direct':
    case 'interpreted':
    case 'confirmed':
    case 'overridden':
    case 'dismissed':
      return trimmed;
    case 'agent_interpreted':
      return 'interpreted';
    default:
      return '';
  }
}

/**
 * Parses URL query parameters on load for `/projects/{id}/decisions`.
 * Invalid values are dropped from the committed filter state so malformed or
 * stale links never fail with HTTP 400 (D10, D11). Under the decisions query dialect,
 * `q` is validated with the `decisions` schema parser/validator and the
 * text-mode 2-rune minimum (200-rune text cap); callers may still read the raw
 * `q` via `extractRawDecisionsQueryParam()` to show it with its inline error
 * in `<scion-filter-box>` while loading the unfiltered list.
 */
export function parseDecisionsUrlState(
  search: string | URLSearchParams,
  opts?: { validateQuery?: (q: string) => boolean }
): DecisionsUrlState {
  const params = typeof search === 'string' ? new URLSearchParams(search) : search;
  const rawTab = (params.get('tab') ?? '').trim();
  const tab: 'pending' | 'made' = rawTab === 'made' ? 'made' : 'pending';

  const q = normalizeDecisionsFilterQuery(params.get('q'), opts?.validateQuery);
  const conversation = cleanConversationValue(params.get('conversation'));
  const rawAgent = cleanFilterValue(params.get('agent'));
  const agent = isValidDecisionAgentId(rawAgent) ? rawAgent : '';

  if (tab !== 'made') {
    return {
      tab,
      q,
      conversation,
      agent,
      user: '',
      provenance: '',
      from: '',
      to: '',
    };
  }

  const user = cleanUserValue(params.get('user'));
  const provenance = cleanProvenanceValue(params.get('provenance'));
  const rawFrom = cleanFilterValue(params.get('from'));
  const rawTo = cleanFilterValue(params.get('to'));
  let from = isValidDecisionCalendarDate(rawFrom) ? rawFrom : '';
  let to = isValidDecisionCalendarDate(rawTo) ? rawTo : '';
  if (from && to && from > to) {
    from = '';
    to = '';
  }

  return {
    tab,
    q,
    conversation,
    agent,
    user,
    provenance,
    from,
    to,
  };
}

/**
 * Serializes current Decisions tab and filter state to a query string (without
 * leading `?`) suitable for `replaceSearch()` (`history.replaceState`, D10).
 * Default `tab=pending` and empty filters are omitted.
 */
export function buildDecisionsUrlSearch(
  state: Partial<DecisionsUrlState>,
  opts?: { validateQuery?: (q: string) => boolean }
): string {
  const params = new URLSearchParams();
  const tab = state.tab === 'made' ? 'made' : 'pending';
  if (tab === 'made') {
    params.set('tab', 'made');
  }

  const q = normalizeDecisionsFilterQuery(state.q, opts?.validateQuery);
  if (q) params.set('q', q);

  const conversation = cleanConversationValue(state.conversation);
  if (conversation) params.set('conversation', conversation);

  const agent = cleanFilterValue(state.agent);
  if (agent) params.set('agent', agent);

  if (tab === 'made') {
    const user = cleanUserValue(state.user);
    if (user) params.set('user', user);

    const provenance = cleanProvenanceValue(state.provenance);
    if (provenance) params.set('provenance', provenance);

    const from = cleanFilterValue(state.from);
    if (from && isValidDecisionCalendarDate(from)) params.set('from', from);

    const to = cleanFilterValue(state.to);
    if (to && isValidDecisionCalendarDate(to)) params.set('to', to);
  }

  return params.toString();
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

  if (options.filter) {
    const q = normalizeDecisionsFilterQuery(options.filter.q, options.validateQuery);
    if (q) params.set('q', q);

    const conversation = cleanConversationValue(options.filter.conversation);
    if (conversation) params.set('conversation', conversation);

    const agent = cleanFilterValue(options.filter.agent);
    if (agent) params.set('agent', agent);

    if (tab === 'made') {
      const user = cleanFilterValue(options.filter.user);
      if (user) params.set('user', user);

      const provenance = cleanFilterValue(options.filter.provenance);
      if (provenance) params.set('provenance', provenance);

      const from = cleanFilterValue(options.filter.from);
      if (from) params.set('from', from);

      const to = cleanFilterValue(options.filter.to);
      if (to) params.set('to', to);
    }
  }

  return `?${params.toString()}`;
}

function projectDecisionsUrl(projectId: string, options: ProjectDecisionsQueryOptions): string {
  return `/api/v1/projects/${encodeURIComponent(projectId)}/decisions${buildProjectDecisionsQuery(options)}`;
}

async function throwDecisionsApiError(res: Response): Promise<never> {
  const info = await parseApiError(res, `HTTP ${res.status}`);
  throw new DecisionsApiError(info.message, res.status, info.code);
}

/** Fetches `GET /api/v1/projects/{id}/decisions?tab=pending`. */
export async function fetchProjectPendingDecisions(
  projectId: string,
  options: {
    cursor?: string | undefined;
    limit?: number | undefined;
    filter?: SharedDecisionsFilter | undefined;
    signal?: AbortSignal | undefined;
    validateQuery?: ((q: string) => boolean) | undefined;
  } = {}
): Promise<ProjectPendingDecisionsResponse> {
  const url = projectDecisionsUrl(projectId, {
    tab: 'pending',
    cursor: options.cursor,
    limit: options.limit,
    filter: options.filter,
    validateQuery: options.validateQuery,
  });
  const res = await apiFetch(url, options.signal ? { signal: options.signal } : undefined);
  if (!res.ok) {
    return throwDecisionsApiError(res);
  }
  const data = (await res.json()) as Partial<ProjectPendingDecisionsResponse> | null;
  return {
    counts: {
      waitingOnYou: data?.counts?.waitingOnYou ?? 0,
      waitingOnOthers: data?.counts?.waitingOnOthers ?? 0,
    },
    ...(data?.filteredCounts
      ? {
          filteredCounts: {
            waitingOnYou: data.filteredCounts.waitingOnYou ?? 0,
            waitingOnOthers: data.filteredCounts.waitingOnOthers ?? 0,
          },
        }
      : {}),
    prompts: Array.isArray(data?.prompts) ? data.prompts : [],
    agents: Array.isArray(data?.agents) ? data.agents : [],
    answerers: Array.isArray(data?.answerers) ? data.answerers : [],
    ...(Array.isArray(data?.conversations) ? { conversations: data.conversations } : {}),
    ...(data?.nextCursor ? { nextCursor: data.nextCursor } : {}),
  };
}

/** Fetches `GET /api/v1/projects/{id}/decisions?tab=made`. */
export async function fetchProjectMadeDecisions(
  projectId: string,
  filter: MadeDecisionsFilter = {},
  options: {
    cursor?: string | undefined;
    limit?: number | undefined;
    signal?: AbortSignal | undefined;
    validateQuery?: ((q: string) => boolean) | undefined;
  } = {}
): Promise<ProjectMadeDecisionsResponse> {
  const url = projectDecisionsUrl(projectId, {
    tab: 'made',
    cursor: options.cursor,
    limit: options.limit,
    filter,
    validateQuery: options.validateQuery,
  });
  const res = await apiFetch(url, options.signal ? { signal: options.signal } : undefined);
  if (!res.ok) {
    return throwDecisionsApiError(res);
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
    ...(Array.isArray(data?.conversations) ? { conversations: data.conversations } : {}),
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
