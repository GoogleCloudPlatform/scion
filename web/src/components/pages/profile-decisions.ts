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
 * Profile Decisions page (`/profile/decisions`, ptone/scion#7996, Design §4.3 R1–R7, D5, D10, D13, D14, §5.1, §5.3, §7).
 *
 * Gated on both `web.project_decisions` and `web.chat_choices` (and falls back
 * to `<scion-page-404>` when the Hub returns 404 on `/api/v1/users/me/decisions`).
 *
 * Two tabs across the caller's member projects:
 *  - "Waiting on you" (`pending`, default `sort=project`): project-grouped or
 *    chronological answerable prompt cards (`surface="decisions_page"`), plus
 *    collapsed "Waiting on others".
 *  - "Made" (`made`, default `sort=recent`): chronological paged log with
 *    Project column (`sort=recent`) or 5-newest-per-project overview groups
 *    with "See all in <project>" drill-down (`sort=project`).
 */

import { LitElement, html, css, nothing } from 'lit';
import type { TemplateResult } from 'lit';
import { customElement, property, state } from 'lit/decorators.js';
import { repeat } from 'lit/directives/repeat.js';

import type { DecisionView, PageData } from '../../shared/types.js';
import { apiFetch } from '../../client/api.js';
import { chatConversationPath } from '../../client/chat-routes.js';
import {
  ANSWERABLE,
  GENERIC_ANSWER_DENIED_REASON,
  actionableReason,
  patchDecisions,
} from '../../client/chat-choices.js';
import type { AskerAnswerability, ChoiceAnsweredDetail } from '../../client/chat-choices.js';
import { navigateTo, replaceSearch } from '../../client/navigation.js';
import { dispatchPageTitle } from '../../client/page-title.js';
import {
  DecisionsApiError,
  hasDecisionsSearchCapability,
  isDecisionsSearchTimeoutError,
  normalizeDecisionsSearchQuery,
} from '../../client/project-decisions.js';
import type {
  MadeDecisionsProvenanceFilter,
  ProjectDecisionActorView,
  ProjectDecisionConversationView,
  ProjectDecisionCounts,
  ProjectDecisionLogEntryView,
  ProjectDecisionProjectRef,
  ProjectDecisionPromptView,
} from '../../client/project-decisions.js';
import {
  buildProfileDecisionsUrlSearch,
  cleanProfileProjectValue,
  defaultProfileDecisionsSort,
  dispatchProfileDecisionsCountChanged,
  fetchProfileMadeDecisions,
  fetchProfilePendingDecisions,
  parseProfileDecisionsUrlState,
} from '../../client/profile-decisions.js';
import type {
  ProfileDecisionActorView,
  ProfileDecisionMadeGroupView,
  ProfileDecisionsSort,
  ProfileMadeDecisionsFilter,
  ProfileSharedDecisionsFilter,
} from '../../client/profile-decisions.js';
import { stateManager } from '../../client/state.js';
import { DisplayZoneController } from '../../utils/display-zone-controller.js';
import { FEATURE_FLAGS_CHANGED_EVENT, projectDecisionsEnabled } from '../../utils/feature-flags.js';
import {
  DISPLAY_TIMEZONE_CHANGED_EVENT,
  effectiveTimeZone,
  formatInstant,
  formatInstantWithZone,
  formatRelative,
  parseWallClock,
} from '../../utils/time.js';
import '../shared/chat/chat-choices.js';
import { deepActiveElement } from '../shared/deep-active-element.js';
import type {
  DecisionsFilterPatch,
  DecisionsFilterTag,
  DecisionsSearchInputDetail,
  ScionDecisionsFilterPanel,
} from '../shared/decisions-filter-panel.js';
import '../shared/decisions-filter-panel.js';
import type { FacetChangeDetail, FacetComboboxOption } from '../shared/facet-combobox.js';
import './not-found.js';

interface WaitingOnOthersItem {
  prompt: ProjectDecisionPromptView;
  decision: DecisionView;
  reason: string;
}

interface PendingProjectGroup {
  project: ProjectDecisionProjectRef;
  prompts: ProjectDecisionPromptView[];
  decisionCount: number;
}

const AVATAR_PALETTE = ['#7c3aed', '#059669', '#0284c7', '#d97706', '#e11d48', '#4f46e5'] as const;
const DEFAULT_DECISIONS_PAGE_SIZE = 50;
const MAX_DECISIONS_PAGE_SIZE = 200;
const SEARCH_DEBOUNCE_MS = 300;

type DecisionsLoadMode = 'reset' | 'append' | 'live';

const EDITABLE_TAG_NAMES = new Set([
  'input',
  'textarea',
  'select',
  'sl-input',
  'sl-textarea',
  'sl-select',
]);

function isEditableTarget(el: EventTarget | null): boolean {
  if (!(el instanceof HTMLElement)) return false;
  if (el.isContentEditable) return true;
  return EDITABLE_TAG_NAMES.has(el.tagName.toLowerCase());
}

function nextCalendarDate(ymd: string): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(ymd);
  if (!match) return '';
  const y = Number(match[1]);
  const m = Number(match[2]);
  const day = Number(match[3]);
  const d = new Date(0);
  d.setUTCFullYear(y, m - 1, day + 1);
  d.setUTCHours(0, 0, 0, 0);
  const ny = String(d.getUTCFullYear()).padStart(4, '0');
  const nm = String(d.getUTCMonth() + 1).padStart(2, '0');
  const nd = String(d.getUTCDate()).padStart(2, '0');
  return `${ny}-${nm}-${nd}`;
}

function dateBoundToIso(ymd: string, endOfDay: boolean): string {
  if (!ymd) return '';
  const zone = effectiveTimeZone();
  if (!endOfDay) {
    return parseWallClock(`${ymd}T00:00`, zone) || ymd;
  }
  const nextYmd = nextCalendarDate(ymd);
  const nextStartIso = nextYmd ? parseWallClock(`${nextYmd}T00:00`, zone) : '';
  if (nextStartIso) {
    const ms = Date.parse(nextStartIso);
    if (Number.isFinite(ms)) {
      return new Date(ms - 1).toISOString();
    }
  }
  return ymd;
}

function avatarColor(seed: string): string {
  let hash = 0;
  for (let i = 0; i < seed.length; i++) {
    hash = (hash * 31 + seed.charCodeAt(i)) | 0;
  }
  const idx = Math.abs(hash) % AVATAR_PALETTE.length;
  return AVATAR_PALETTE[idx] ?? AVATAR_PALETTE[0];
}

function avatarInitial(name: string): string {
  const ch = (name || '?').trim().charAt(0).toUpperCase();
  return ch || '?';
}

function surfaceDisplay(surface: string): { icon: string; label: string } {
  switch (surface) {
    case 'decisions_page':
      return { icon: 'window', label: 'Decisions page' };
    case 'native':
    case 'web':
      return { icon: 'chat-dots', label: 'Chat' };
    case 'slack':
      return { icon: 'slack', label: 'Slack' };
    case 'google_chat':
      return { icon: 'chat-dots', label: 'Google Chat' };
    case 'discord':
      return { icon: 'discord', label: 'Discord' };
    case 'telegram':
      return { icon: 'telegram', label: 'Telegram' };
    case 'teams':
      return { icon: 'microsoft-teams', label: 'Teams' };
    case 'other':
    case '':
      return { icon: '', label: 'Other' };
    default:
      return { icon: '', label: surface };
  }
}

@customElement('scion-page-profile-decisions')
export class ScionPageProfileDecisions extends LitElement {
  readonly _zone = new DisplayZoneController(this);

  @property({ type: Object })
  pageData: PageData | null = null;

  @state() private enabled = false;
  @state() private hubNotFound = false;
  @state() private currentUserId = '';
  @state() private activeTab: 'pending' | 'made' = 'pending';
  @state() private sort: ProfileDecisionsSort = 'project';

  @state() private initialLoading = true;
  @state() private error: string | null = null;

  @state() private counts: ProjectDecisionCounts = { waitingOnYou: 0, waitingOnOthers: 0 };
  @state() private filteredCounts: ProjectDecisionCounts | null = null;
  @state() private supportsSearch = true;
  @state() private truncatedProjects = false;

  @state() private pendingPrompts: ProjectDecisionPromptView[] = [];
  @state() private pendingNextCursor = '';
  @state() private pendingLoadingMore = false;
  @state() private pendingSearchTimeout = false;
  @state() private pendingLoading = false;

  @state() private madeLoaded = false;
  @state() private madeLoading = false;
  @state() private madeLoadingMore = false;
  @state() private madeError: string | null = null;
  @state() private madeSearchTimeout = false;
  @state() private madeEntries: ProjectDecisionLogEntryView[] = [];
  @state() private madeGroups: ProfileDecisionMadeGroupView[] = [];
  @state() private madeNextCursor = '';

  @state() private facetsAgents: ProfileDecisionActorView[] = [];
  @state() private facetsAnswerers: ProfileDecisionActorView[] = [];
  @state() private facetsConversations: ProjectDecisionConversationView[] = [];
  @state() private facetsProjects: ProjectDecisionProjectRef[] = [];

  @state() private filterProject = '';
  @state() private filterQ = '';
  @state() private searchDraft = '';
  @state() private filterConversation = '';
  @state() private filterAgent = '';
  @state() private filterUser = '';
  @state() private filterProvenance: MadeDecisionsProvenanceFilter = '';
  @state() private filterFrom = '';
  @state() private filterTo = '';

  private pendingSortOverride: ProfileDecisionsSort | null = null;
  private madeSortOverride: ProfileDecisionsSort | null = null;
  private knownProjectsById = new Map<string, ProjectDecisionProjectRef>();

  private pendingGen = 0;
  private madeGen = 0;
  private pendingAbort: AbortController | null = null;
  private madeAbort: AbortController | null = null;
  private userAbort: AbortController | null = null;
  private liveRefreshTimer: ReturnType<typeof setTimeout> | null = null;
  private searchDebounceTimer: ReturnType<typeof setTimeout> | null = null;
  private pendingResetInFlight = false;
  private pendingRefreshDeferred = false;
  private pendingNeedsReset = false;
  private pendingPagedBeyondFirst = false;
  private madePagedBeyondFirst = false;
  private madeRefreshDeferred = false;
  private madeStale = false;
  private madeNeedsReset = false;
  private facetsHydrated = false;
  private booted = false;

  static override styles = css`
    :host {
      display: block;
      --chat-fs-sm: 0.875rem;
      --chat-fs-base: 0.9375rem;
      --chat-fs-lg: 1.0625rem;
    }

    .ph {
      display: flex;
      align-items: center;
      gap: 0.75rem;
      flex-wrap: wrap;
    }

    .ph h1 {
      margin: 0;
      font-size: 1.5rem;
      font-weight: 700;
      color: var(--scion-text, #1e293b);
    }

    .sub {
      color: var(--scion-text-secondary, #475569);
      font-size: 0.875rem;
      margin: 0.25rem 0 1rem;
    }

    sl-tab sl-badge::part(base) {
      font-size: 0.68rem;
      margin-left: 0.35rem;
    }

    sl-tab-panel::part(base) {
      padding: 0.75rem 0 0;
    }

    .sort-bar {
      display: flex;
      align-items: center;
      gap: 0.6rem;
      flex-wrap: wrap;
      margin: 0 0 0.25rem;
    }

    .sort-bar sl-radio-button::part(button) {
      background: var(--scion-surface, #ffffff);
      color: var(--scion-text, #1e293b);
      border-color: var(--scion-border, #e2e8f0);
    }

    .sort-bar sl-radio-button[checked]::part(button) {
      background: var(--scion-primary, #3b82f6);
      color: #ffffff;
      border-color: var(--scion-primary, #3b82f6);
    }

    scion-decisions-filter-panel > scion-facet-combobox[slot='leading-controls'] {
      flex: 1 1 11rem;
      min-width: 10rem;
    }

    sl-alert[data-truncated-projects],
    sl-alert[data-search-timeout] {
      display: block;
      margin: 0.75rem 0;
      --sl-panel-background-color: var(--scion-surface, #ffffff);
      --sl-panel-border-color: var(--scion-border, #e2e8f0);
      --sl-color-neutral-700: var(--scion-text, #1e293b);
    }

    sl-badge[variant='neutral']::part(base) {
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      border-color: var(--scion-border, #e2e8f0);
    }

    .groupsec {
      margin: 1rem 0 0.35rem;
      display: flex;
      align-items: center;
      gap: 0.5rem;
      flex-wrap: wrap;
    }

    .groupsec h3 {
      margin: 0;
      font-size: 0.95rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .groupsec .right {
      margin-left: auto;
      font-size: 0.8125rem;
    }

    .loading-state {
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: center;
      padding: 4rem 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .loading-state sl-spinner {
      font-size: 2rem;
      margin-bottom: 1rem;
    }

    .error-state {
      text-align: center;
      padding: 3rem 2rem;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    .error-state sl-icon {
      font-size: 2.5rem;
      color: var(--sl-color-danger-500, #ef4444);
      margin-bottom: 0.75rem;
    }

    .error-state h2 {
      font-size: 1.2rem;
      font-weight: 600;
      color: var(--scion-text, #1e293b);
      margin: 0 0 0.5rem;
    }

    .error-details {
      font-family: var(--scion-font-mono, monospace);
      font-size: 0.85rem;
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.65rem 0.9rem;
      border-radius: var(--scion-radius, 0.5rem);
      color: var(--sl-color-danger-700, #b91c1c);
      margin-bottom: 1rem;
    }

    /* ---- Prompt cards (Mocks R-1, R-2) ---- */
    .pcard {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      margin: 0.75rem 0;
    }

    .pcard-head {
      display: flex;
      align-items: center;
      gap: 0.5rem;
      padding: 0.55rem 0.75rem;
      font-size: var(--chat-fs-sm, 0.875rem);
      color: var(--scion-text-secondary, #475569);
      flex-wrap: wrap;
    }

    .pcard-head .avatar {
      width: 1.5rem;
      height: 1.5rem;
      border-radius: 50%;
      display: inline-flex;
      align-items: center;
      justify-content: center;
      font-size: 0.72rem;
      font-weight: 700;
      color: #ffffff;
      flex-shrink: 0;
    }

    .pcard-head b {
      color: var(--scion-text, #1e293b);
    }

    .pcard-head .right {
      margin-left: auto;
      display: inline-flex;
      gap: 0.4rem;
      align-items: center;
    }

    .pcard scion-chat-choices {
      margin: 0 0.75rem 0.75rem;
    }

    .linkish {
      color: var(--scion-primary, #3b82f6);
      text-decoration: none;
      font-weight: 500;
      cursor: pointer;
      display: inline-flex;
      align-items: center;
      gap: 0.3rem;
    }

    .linkish:hover {
      text-decoration: underline;
    }

    .purged-note {
      display: inline-flex;
      align-items: center;
      gap: 0.3rem;
      color: var(--scion-text-muted, #64748b);
    }

    .muted {
      color: var(--scion-text-muted, #64748b);
    }

    .key {
      font: 600 0.75rem/1 var(--scion-font-mono, monospace);
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      padding: 0.2rem 0.4rem;
      border-radius: 0.25rem;
      white-space: nowrap;
    }

    .badge {
      display: inline-flex;
      align-items: center;
      gap: 0.2rem;
      font-size: 0.68rem;
      font-weight: 600;
      line-height: 1;
      padding: 0.2rem 0.45rem;
      border-radius: 999px;
      background: var(--scion-badge-neutral-bg, #f1f5f9);
      color: var(--scion-badge-neutral-text, #334155);
      white-space: nowrap;
    }

    .badge.interp {
      background: var(--scion-badge-warning-bg, #fef3c7);
      color: var(--scion-badge-warning-text, #92400e);
    }

    .badge.conf {
      background: var(--scion-badge-success-bg, #dcfce7);
      color: var(--scion-badge-success-text, #166534);
    }

    .badge.over {
      background: var(--scion-badge-primary-bg, #dbeafe);
      color: var(--scion-badge-primary-text, #1e40af);
    }

    .badge sl-icon {
      font-size: 0.8rem;
    }

    sl-details.others {
      margin-top: 1rem;
    }

    sl-details.others::part(base) {
      background: var(--scion-surface, #ffffff);
      border-color: var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
    }

    sl-details.others::part(header) {
      font-weight: 600;
      color: var(--scion-text, #1e293b);
    }

    .mlist .item {
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius, 0.5rem);
      padding: 0.55rem 0.65rem;
      margin: 0.5rem 0;
      font-size: 0.85rem;
      color: var(--scion-text, #1e293b);
    }

    .mlist .item .top {
      display: flex;
      gap: 0.4rem;
      align-items: center;
      flex-wrap: wrap;
      margin-bottom: 0.15rem;
    }

    .mlist .item .meta {
      color: var(--scion-text-muted, #64748b);
      font-size: 0.78rem;
      margin-top: 0.25rem;
    }

    .date-range-error {
      display: flex;
      align-items: center;
      gap: 0.45rem;
      padding: 0.55rem 0.75rem;
      margin: 0 0 0.75rem;
      font-size: 0.85rem;
      color: var(--sl-color-danger-700, #b91c1c);
      background: var(--sl-color-danger-50, #fef2f2);
      border: 1px solid var(--sl-color-danger-200, #fecaca);
      border-radius: var(--scion-radius, 0.5rem);
    }

    .date-range-error sl-icon {
      color: var(--sl-color-danger-600, #dc2626);
      flex-shrink: 0;
    }

    table.log {
      width: 100%;
      border-collapse: collapse;
      background: var(--scion-surface, #ffffff);
      border: 1px solid var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      overflow: hidden;
      font-size: 0.85rem;
      color: var(--scion-text, #1e293b);
    }

    table.log th {
      text-align: left;
      font-weight: 600;
      font-size: 0.75rem;
      color: var(--scion-text-secondary, #475569);
      background: var(--scion-bg-subtle, #f1f5f9);
      padding: 0.5rem 0.6rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      white-space: nowrap;
    }

    table.log td {
      padding: 0.5rem 0.6rem;
      border-bottom: 1px solid var(--scion-border, #e2e8f0);
      vertical-align: top;
    }

    table.log tbody tr:last-child td {
      border-bottom: none;
    }

    table.log tr.follow td {
      background: var(--scion-bg, #f8fafc);
    }

    table.log td.q {
      max-width: 18rem;
      font-weight: 400;
    }

    table.log td.q .q-text {
      display: -webkit-box;
      -webkit-line-clamp: 2;
      -webkit-box-orient: vertical;
      overflow: hidden;
      overflow-wrap: anywhere;
    }

    .surf {
      display: inline-flex;
      gap: 0.3rem;
      align-items: center;
      color: var(--scion-text-secondary, #475569);
      font-size: 0.8rem;
      white-space: nowrap;
    }

    .made-mobile-list {
      display: none;
    }

    .more {
      display: flex;
      justify-content: center;
      margin: 0.85rem 0;
    }

    .empty {
      text-align: center;
      padding: 2.5rem 1rem;
      color: var(--scion-text-secondary, #475569);
      background: var(--scion-surface, #ffffff);
      border: 1px dashed var(--scion-border, #e2e8f0);
      border-radius: var(--scion-radius-lg, 0.75rem);
      margin: 0.5rem 0;
    }

    .empty sl-icon {
      font-size: 2rem;
      color: var(--scion-text-muted, #64748b);
    }

    .empty p {
      margin: 0.4rem 0 0;
    }

    .empty p b {
      color: var(--scion-text, #1e293b);
    }

    @media (max-width: 760px) {
      scion-decisions-filter-panel > scion-facet-combobox[slot='leading-controls'] {
        flex: 1 1 auto;
        width: 100%;
        min-width: 0;
      }
    }

    @media (max-width: 640px) {
      .ph h1 {
        font-size: 1.2rem;
      }

      table.log {
        display: none;
      }

      .made-mobile-list {
        display: block;
      }
    }
  `;

  private readonly boundOnFlagsChanged = (): void => {
    const next = projectDecisionsEnabled();
    if (next === this.enabled) return;
    this.enabled = next;
    if (!next) {
      this.teardownActiveLoads();
      this.booted = false;
      return;
    }
    void this.bootIfEnabled();
  };

  private readonly boundOnChoiceUpdated = (e: Event): void => {
    if (!this.enabled || this.hubNotFound) return;
    const raw = (e as CustomEvent).detail as {
      data?: {
        projectId?: string;
        messageId?: string;
        decision?: DecisionView;
      };
      projectId?: string;
      messageId?: string;
      decision?: DecisionView;
    } | null;
    const data = raw?.data ?? raw;
    if (data?.decision) {
      this.applyIncomingDecisionPatch(data.messageId, [data.decision]);
    }
    this.scheduleLiveRefresh();
  };

  private readonly boundOnChatMessageReceived = (e: Event): void => {
    if (!this.enabled || this.hubNotFound) return;
    const raw = (e as CustomEvent).detail as {
      data?: { type?: string; decisions?: unknown; message?: { choices?: unknown } };
      type?: string;
      decisions?: unknown;
      message?: { choices?: unknown };
    } | null;
    const data = raw?.data ?? raw;
    if (
      data?.type !== 'choice-prompt' &&
      !Array.isArray(data?.decisions) &&
      !data?.message?.choices
    ) {
      return;
    }
    this.scheduleLiveRefresh();
  };

  private readonly boundOnTimezoneChanged = (): void => {
    if (!this.enabled || !this.madeLoaded || (!this.filterFrom && !this.filterTo)) return;
    if (this.hasDateRangeError) return;
    if (this.activeTab === 'made') {
      void this.loadMadePage({ mode: 'reset' });
    } else {
      this.madeStale = true;
    }
  };

  private readonly boundOnKeydown = (e: KeyboardEvent): void => {
    if (e.key !== '/' || e.defaultPrevented || e.metaKey || e.ctrlKey || e.altKey) return;
    if (
      !this.enabled ||
      this.hubNotFound ||
      !this.supportsSearch ||
      this.initialLoading ||
      this.error !== null
    ) {
      return;
    }
    if (isEditableTarget(deepActiveElement())) return;
    for (const target of e.composedPath()) {
      if (isEditableTarget(target)) return;
    }
    const panel = this.shadowRoot?.querySelector<ScionDecisionsFilterPanel>(
      'scion-decisions-filter-panel'
    );
    if (!panel) return;
    e.preventDefault();
    void panel.expandAndFocusSearch();
  };

  override connectedCallback(): void {
    super.connectedCallback();
    this.extractRouteState();
    this.enabled = projectDecisionsEnabled();
    window.addEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    window.addEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, this.boundOnTimezoneChanged);
    window.addEventListener('keydown', this.boundOnKeydown);
    stateManager.addEventListener('choice-updated', this.boundOnChoiceUpdated);
    stateManager.addEventListener('chat-message-received', this.boundOnChatMessageReceived);
    void this.bootIfEnabled();
  }

  override disconnectedCallback(): void {
    super.disconnectedCallback();
    window.removeEventListener(FEATURE_FLAGS_CHANGED_EVENT, this.boundOnFlagsChanged);
    window.removeEventListener(DISPLAY_TIMEZONE_CHANGED_EVENT, this.boundOnTimezoneChanged);
    window.removeEventListener('keydown', this.boundOnKeydown);
    stateManager.removeEventListener('choice-updated', this.boundOnChoiceUpdated);
    stateManager.removeEventListener('chat-message-received', this.boundOnChatMessageReceived);
    this.teardownActiveLoads();
  }

  private teardownActiveLoads(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
      this.liveRefreshTimer = null;
    }
    if (this.searchDebounceTimer !== null) {
      clearTimeout(this.searchDebounceTimer);
      this.searchDebounceTimer = null;
    }
    this.pendingResetInFlight = false;
    this.pendingLoading = false;
    this.pendingLoadingMore = false;
    this.pendingRefreshDeferred = false;
    this.madeLoading = false;
    this.madeLoadingMore = false;
    this.madeRefreshDeferred = false;
    this.pendingAbort?.abort();
    this.pendingAbort = null;
    this.madeAbort?.abort();
    this.madeAbort = null;
    this.userAbort?.abort();
    this.userAbort = null;
  }

  private extractRouteState(): void {
    const rawPath = this.pageData?.path || window.location.pathname + window.location.search;
    const [, query = ''] = rawPath.split('?', 2);
    if (this.pageData?.user?.id) {
      this.currentUserId = this.pageData.user.id;
    }
    const search = query ? `?${query}` : window.location.search;
    const parsed = parseProfileDecisionsUrlState(search);
    this.activeTab = parsed.tab;
    this.sort = parsed.sort;
    if (parsed.tab === 'pending') {
      this.pendingSortOverride =
        parsed.sort !== defaultProfileDecisionsSort('pending') ? parsed.sort : null;
      this.madeSortOverride = null;
    } else {
      this.madeSortOverride =
        parsed.sort !== defaultProfileDecisionsSort('made') ? parsed.sort : null;
      this.pendingSortOverride = null;
    }
    this.filterProject = parsed.project;
    this.filterQ = parsed.q;
    this.searchDraft = parsed.q;
    this.filterConversation = parsed.conversation;
    this.filterAgent = parsed.agent;
    this.filterUser = parsed.user;
    this.filterProvenance = parsed.provenance;
    this.filterFrom = parsed.from;
    this.filterTo = parsed.to;
  }

  private syncUrlState(): void {
    const nextQs = buildProfileDecisionsUrlSearch({
      tab: this.activeTab,
      sort: this.sort,
      project: this.filterProject,
      q: this.filterQ,
      conversation: this.filterConversation,
      agent: this.filterAgent,
      user: this.filterUser,
      provenance: this.filterProvenance,
      from: this.filterFrom,
      to: this.filterTo,
    });
    const curQs = window.location.search.replace(/^\?/, '');
    if (nextQs !== curQs) {
      replaceSearch(nextQs);
    }
  }

  private async bootIfEnabled(): Promise<void> {
    if (!this.enabled || this.booted) return;
    this.extractRouteState();
    this.syncUrlState();
    this.booted = true;
    this.hubNotFound = false;
    this.initialLoading = true;
    this.error = null;
    dispatchPageTitle(this, 'Decisions', 'Profile');

    if (this.currentUserId) {
      stateManager.setCurrentUserId(this.currentUserId);
    }
    stateManager.setScope({ type: 'dashboard' });

    const userTask = this.currentUserId ? Promise.resolve() : this.loadCurrentUserBestEffort();
    const pendingTask = this.loadPendingPage({ mode: 'reset' });
    const madeTask =
      this.activeTab === 'made' ? this.loadMadePage({ mode: 'reset' }) : Promise.resolve();

    await Promise.all([userTask, pendingTask, madeTask]);
    if (this.isConnected) {
      this.initialLoading = false;
    }
  }

  private async loadCurrentUserBestEffort(): Promise<void> {
    this.userAbort?.abort();
    const ac = new AbortController();
    this.userAbort = ac;
    try {
      const res = await apiFetch('/auth/me', { authErrors: 'caller', signal: ac.signal });
      if (ac.signal.aborted || !res.ok || !this.isConnected) return;
      const body = (await res.json()) as { id?: string; user?: { id?: string } } | null;
      if (ac.signal.aborted || !this.isConnected) return;
      const uid = body?.id || body?.user?.id || '';
      if (uid && !this.currentUserId) {
        this.currentUserId = uid;
        stateManager.setCurrentUserId(uid);
        if (this.enabled) {
          stateManager.setScope({ type: 'dashboard' });
        }
      }
    } catch {
      // Best-effort fallback when pageData.user was not injected.
    } finally {
      if (this.userAbort === ac) {
        this.userAbort = null;
      }
    }
  }

  private recordKnownProjects(projects: readonly ProjectDecisionProjectRef[]): void {
    for (const p of projects) {
      if (p?.id) {
        this.knownProjectsById.set(p.id, p);
      }
    }
  }

  private recordKnownProjectsFromPrompts(prompts: readonly ProjectDecisionPromptView[]): void {
    for (const p of prompts) {
      if (p.project?.id) {
        this.knownProjectsById.set(p.project.id, p.project);
      }
    }
  }

  private recordKnownProjectsFromEntries(
    entries: readonly ProjectDecisionLogEntryView[],
    fallbackProject?: ProjectDecisionProjectRef
  ): void {
    for (const e of entries) {
      const proj = e.project?.id ? e.project : fallbackProject;
      if (proj?.id) {
        this.knownProjectsById.set(proj.id, proj);
      }
    }
  }

  private projectDisplayName(ref: ProjectDecisionProjectRef | undefined, idFallback = ''): string {
    if (ref?.name) return ref.name;
    if (ref?.slug) return ref.slug;
    const id = ref?.id || idFallback;
    if (!id) return '';
    const known = this.knownProjectsById.get(id);
    return known?.name || known?.slug || id;
  }

  private mergeFacets(
    agents: ProfileDecisionActorView[],
    answerers: ProfileDecisionActorView[],
    projects: ProjectDecisionProjectRef[],
    conversations?: ProjectDecisionConversationView[]
  ): void {
    this.facetsHydrated = true;
    this.recordKnownProjects(projects);
    if (projects.length > 0 || this.facetsProjects.length === 0) {
      this.facetsProjects = projects;
    }
    if (agents.length > 0 || this.facetsAgents.length === 0) {
      this.facetsAgents = agents;
    }
    if (answerers.length > 0 || this.facetsAnswerers.length === 0) {
      this.facetsAnswerers = answerers;
    }
    if (hasDecisionsSearchCapability({ conversations }) && conversations) {
      this.supportsSearch = true;
      if (conversations.length > 0 || this.facetsConversations.length === 0) {
        this.facetsConversations = conversations;
      }
    }
  }

  private updateCounts(next: ProjectDecisionCounts): void {
    this.counts = next;
    dispatchProfileDecisionsCountChanged(next.waitingOnYou);
  }

  private currentSharedFilter(): ProfileSharedDecisionsFilter {
    return {
      q: this.filterQ,
      conversation: this.filterConversation,
      agent: this.filterAgent,
      project: this.filterProject,
    };
  }

  private get isPendingFiltered(): boolean {
    return Boolean(
      (this.supportsSearch &&
        (normalizeDecisionsSearchQuery(this.filterQ) || this.filterConversation)) ||
      this.filterAgent ||
      this.filterProject
    );
  }

  private get isMadeFiltered(): boolean {
    return Boolean(
      (this.supportsSearch &&
        (normalizeDecisionsSearchQuery(this.filterQ) || this.filterConversation)) ||
      this.filterAgent ||
      this.filterProject ||
      this.filterUser ||
      this.filterProvenance ||
      this.filterFrom ||
      this.filterTo
    );
  }

  private pendingEffectiveSort(): ProfileDecisionsSort {
    return this.activeTab === 'pending'
      ? this.sort
      : (this.pendingSortOverride ?? defaultProfileDecisionsSort('pending'));
  }

  private madeEffectiveSort(): ProfileDecisionsSort {
    return this.activeTab === 'made'
      ? this.sort
      : (this.madeSortOverride ?? defaultProfileDecisionsSort('made'));
  }

  private async loadPendingPage(opts: { mode: DecisionsLoadMode }): Promise<void> {
    if (!this.enabled || this.hubNotFound) return;
    if (opts.mode === 'live' && (this.pendingLoadingMore || this.pendingResetInFlight)) {
      this.pendingRefreshDeferred = true;
      return;
    }
    if (opts.mode !== 'live') {
      this.pendingRefreshDeferred = false;
    }
    this.pendingNeedsReset = false;

    const gen = ++this.pendingGen;
    this.pendingAbort?.abort();
    const ac = new AbortController();
    this.pendingAbort = ac;

    const effectiveSort = this.pendingEffectiveSort();
    const cursor = opts.mode === 'append' ? this.pendingNextCursor : undefined;
    const limit =
      opts.mode === 'live'
        ? Math.min(
            MAX_DECISIONS_PAGE_SIZE,
            Math.max(DEFAULT_DECISIONS_PAGE_SIZE, this.pendingPrompts.length)
          )
        : undefined;

    if (opts.mode === 'reset') {
      this.pendingResetInFlight = true;
      this.pendingLoading = !this.initialLoading;
      this.pendingSearchTimeout = false;
    } else if (opts.mode === 'append') {
      this.pendingLoadingMore = true;
    }

    try {
      const res = await fetchProfilePendingDecisions({
        sort: effectiveSort,
        cursor,
        limit,
        filter: this.currentSharedFilter(),
        signal: ac.signal,
      });
      if (gen !== this.pendingGen || !this.isConnected) return;
      this.updateCounts(res.counts);
      this.filteredCounts = res.filteredCounts ?? null;
      this.truncatedProjects = res.truncatedProjects;
      this.pendingSearchTimeout = false;
      this.recordKnownProjectsFromPrompts(res.prompts);

      if (opts.mode === 'reset') {
        this.pendingPrompts = res.prompts;
        this.pendingNextCursor = res.nextCursor ?? '';
        this.pendingPagedBeyondFirst = false;
      } else if (opts.mode === 'append') {
        const existingIds = new Set(this.pendingPrompts.map((p) => p.promptMessageId));
        const extra = res.prompts.filter((p) => !existingIds.has(p.promptMessageId));
        this.pendingPrompts = [...this.pendingPrompts, ...extra];
        this.pendingNextCursor = res.nextCursor ?? '';
        this.pendingPagedBeyondFirst = true;
      } else {
        // In sort=project mode, P4's cursor tracks finished projects; always
        // reload from page 1 so project group ranks and cursors stay consistent.
        if (effectiveSort === 'recent' && this.pendingPrompts.length > MAX_DECISIONS_PAGE_SIZE) {
          const headIds = new Set(res.prompts.map((p) => p.promptMessageId));
          const openTail = this.pendingPrompts
            .slice(MAX_DECISIONS_PAGE_SIZE)
            .filter(
              (p) =>
                !headIds.has(p.promptMessageId) &&
                (p.waitingOnYou || this.promptHasWaitingOnOthers(p))
            );
          this.pendingPrompts = [...res.prompts, ...openTail];
        } else {
          this.pendingPrompts = res.prompts;
          this.pendingNextCursor = res.nextCursor ?? '';
          this.pendingPagedBeyondFirst = false;
        }
      }

      this.mergeFacets(res.agents, res.answerers, res.projects, res.conversations);
      if (opts.mode !== 'live') {
        this.error = null;
      }
    } catch (err) {
      if (ac.signal.aborted || gen !== this.pendingGen || !this.isConnected) return;
      if (err instanceof DecisionsApiError && err.status === 404) {
        this.hubNotFound = true;
        this.error = null;
        return;
      }
      if (opts.mode !== 'live') {
        if (isDecisionsSearchTimeoutError(err)) {
          this.supportsSearch = true;
          this.pendingSearchTimeout = true;
          this.error = null;
          if (opts.mode === 'reset' && this.activeTab === 'pending' && !this.facetsHydrated) {
            await this.hydrateFacetsAfterTimeout('pending', ac.signal, gen);
          }
        } else {
          this.pendingSearchTimeout = false;
          this.error = err instanceof Error ? err.message : 'Failed to load decisions';
        }
      }
    } finally {
      if (gen === this.pendingGen) {
        this.pendingResetInFlight = false;
        this.pendingLoading = false;
        this.pendingLoadingMore = false;
        if (this.pendingAbort === ac) this.pendingAbort = null;
        if (this.pendingRefreshDeferred) {
          this.pendingRefreshDeferred = false;
          if (
            this.isConnected &&
            this.enabled &&
            !this.hubNotFound &&
            !this.error &&
            !this.pendingSearchTimeout &&
            !this.pendingNeedsReset
          ) {
            void this.loadPendingPage({ mode: 'live' });
          }
        }
      }
    }
  }

  private async hydrateFacetsAfterTimeout(
    tab: 'pending' | 'made',
    signal: AbortSignal,
    expectedGen: number
  ): Promise<void> {
    if (this.facetsHydrated || !this.isConnected || !this.enabled || this.hubNotFound) return;
    try {
      if (tab === 'pending') {
        const res = await fetchProfilePendingDecisions({
          sort: this.pendingEffectiveSort(),
          limit: 1,
          filter: {
            conversation: this.filterConversation,
            agent: this.filterAgent,
            project: this.filterProject,
          },
          signal,
        });
        if (signal.aborted || expectedGen !== this.pendingGen || !this.isConnected) return;
        this.updateCounts(res.counts);
        this.truncatedProjects = res.truncatedProjects;
        this.recordKnownProjectsFromPrompts(res.prompts);
        this.mergeFacets(res.agents, res.answerers, res.projects, res.conversations);
      } else {
        const res = await fetchProfileMadeDecisions(
          { ...this.currentMadeFilter(), q: '' },
          {
            sort: this.madeEffectiveSort(),
            limit: 1,
            signal,
          }
        );
        if (signal.aborted || expectedGen !== this.madeGen || !this.isConnected) return;
        this.updateCounts(res.counts);
        this.truncatedProjects = res.truncatedProjects;
        if (res.sort === 'project') {
          for (const g of res.groups) {
            this.recordKnownProjectsFromEntries(g.entries, g.project);
          }
        } else {
          this.recordKnownProjectsFromEntries(res.entries);
        }
        this.mergeFacets(res.agents, res.answerers, res.projects, res.conversations);
      }
    } catch {
      // Best-effort facet hydration: preserve SEARCH_TIMEOUT warning alert on fallback failure.
    }
  }

  private promptHasWaitingOnOthers(p: ProjectDecisionPromptView): boolean {
    return p.decisions.some(
      (d) =>
        Boolean(p.waitingReasons?.[d.rowId]) ||
        (!p.waitingOnYou &&
          d.status !== 'replaced' &&
          !(d.status === 'decided' && d.decision?.provenance !== 'agent_interpreted'))
    );
  }

  private get hasDateRangeError(): boolean {
    return Boolean(this.filterFrom && this.filterTo && this.filterFrom > this.filterTo);
  }

  private currentMadeFilter(): ProfileMadeDecisionsFilter {
    return {
      q: this.filterQ,
      conversation: this.filterConversation,
      agent: this.filterAgent,
      project: this.filterProject,
      user: this.filterUser,
      provenance: this.filterProvenance,
      from: dateBoundToIso(this.filterFrom, false),
      to: dateBoundToIso(this.filterTo, true),
    };
  }

  private async loadMadePage(opts: { mode: DecisionsLoadMode }): Promise<void> {
    if (!this.enabled || this.hubNotFound || this.hasDateRangeError) return;
    if (opts.mode === 'live' && (this.madeLoadingMore || this.madeLoading)) {
      this.madeRefreshDeferred = true;
      return;
    }
    if (opts.mode !== 'live') {
      this.madeRefreshDeferred = false;
    }
    this.madeStale = false;
    this.madeNeedsReset = false;

    const gen = ++this.madeGen;
    this.madeAbort?.abort();
    const ac = new AbortController();
    this.madeAbort = ac;

    const effectiveSort = this.madeEffectiveSort();
    const cursor =
      opts.mode === 'append' && effectiveSort === 'recent' ? this.madeNextCursor : undefined;
    const limit = opts.mode === 'live' ? DEFAULT_DECISIONS_PAGE_SIZE : undefined;

    if (opts.mode === 'reset') {
      this.madeLoading = true;
      this.madeError = null;
      this.madeSearchTimeout = false;
    } else if (opts.mode === 'append') {
      this.madeLoadingMore = true;
      this.madeError = null;
    }

    try {
      const res = await fetchProfileMadeDecisions(this.currentMadeFilter(), {
        sort: effectiveSort,
        cursor,
        limit,
        signal: ac.signal,
      });
      if (gen !== this.madeGen || !this.isConnected) return;
      this.updateCounts(res.counts);
      this.truncatedProjects = res.truncatedProjects;
      this.madeLoaded = true;
      this.madeError = null;
      this.madeSearchTimeout = false;

      if (res.sort === 'project') {
        for (const g of res.groups) {
          this.recordKnownProjectsFromEntries(g.entries, g.project);
        }
        this.madeGroups = res.groups;
        this.madeEntries = [];
        this.madeNextCursor = '';
        this.madePagedBeyondFirst = false;
      } else {
        this.recordKnownProjectsFromEntries(res.entries);
        this.madeGroups = [];
        if (opts.mode === 'reset') {
          this.madeEntries = res.entries;
          this.madeNextCursor = res.nextCursor ?? '';
          this.madePagedBeyondFirst = false;
        } else if (opts.mode === 'append') {
          const existingIds = new Set(this.madeEntries.map((e) => e.answerId));
          const extra = res.entries.filter((e) => !existingIds.has(e.answerId));
          this.madeEntries = [...this.madeEntries, ...extra];
          this.madeNextCursor = res.nextCursor ?? '';
          this.madePagedBeyondFirst = true;
        } else {
          const incomingById = new Map(res.entries.map((e) => [e.answerId, e]));
          const existingIds = new Set(this.madeEntries.map((e) => e.answerId));
          const prepended = res.entries.filter((e) => !existingIds.has(e.answerId));
          const updatedExisting = this.madeEntries.map((e) => incomingById.get(e.answerId) ?? e);
          this.madeEntries = [...prepended, ...updatedExisting];
          if (!this.madePagedBeyondFirst && !this.madeNextCursor) {
            this.madeNextCursor = res.nextCursor ?? '';
          }
        }
      }

      this.mergeFacets(res.agents, res.answerers, res.projects, res.conversations);
    } catch (err) {
      if (ac.signal.aborted || gen !== this.madeGen || !this.isConnected) return;
      if (err instanceof DecisionsApiError && err.status === 404) {
        this.hubNotFound = true;
        this.madeError = null;
        return;
      }
      if (opts.mode !== 'live') {
        if (isDecisionsSearchTimeoutError(err)) {
          this.supportsSearch = true;
          this.madeSearchTimeout = true;
          this.madeError = null;
          if (opts.mode === 'reset' && this.activeTab === 'made' && !this.facetsHydrated) {
            await this.hydrateFacetsAfterTimeout('made', ac.signal, gen);
          }
        } else {
          this.madeSearchTimeout = false;
          this.madeError = err instanceof Error ? err.message : 'Failed to load decision log';
        }
      }
    } finally {
      if (gen === this.madeGen) {
        this.madeLoading = false;
        this.madeLoadingMore = false;
        if (this.madeAbort === ac) this.madeAbort = null;
        if (this.madeRefreshDeferred) {
          this.madeRefreshDeferred = false;
          if (
            this.isConnected &&
            this.enabled &&
            !this.hubNotFound &&
            !this.hasDateRangeError &&
            !this.madeError &&
            !this.madeSearchTimeout &&
            !this.madeNeedsReset
          ) {
            if (this.activeTab === 'made') {
              void this.loadMadePage({ mode: 'live' });
            } else if (this.madeLoaded) {
              this.madeStale = true;
            }
          }
        }
      }
    }
  }

  private applyIncomingDecisionPatch(
    messageId: string | undefined,
    incoming: readonly DecisionView[]
  ): void {
    if (incoming.length === 0 || this.pendingPrompts.length === 0) return;
    const incomingIds = new Set(incoming.map((d) => d.rowId));
    let changed = false;
    let deltaWaitingOnYou = 0;

    const nextPrompts = this.pendingPrompts.map((p) => {
      const matches =
        (messageId && p.promptMessageId === messageId) ||
        p.decisions.some((d) => incomingIds.has(d.rowId));
      if (!matches) return p;

      const patched = patchDecisions(p.decisions, incoming);
      if (patched === p.decisions) return p;
      changed = true;

      const wasActionableCount = p.waitingOnYou
        ? p.decisions.filter(
            (d) => !p.waitingReasons?.[d.rowId] && actionableReason(d, this.currentUserId) === ''
          ).length
        : 0;
      const nowActionableCount = p.waitingOnYou
        ? patched.filter(
            (d) => !p.waitingReasons?.[d.rowId] && actionableReason(d, this.currentUserId) === ''
          ).length
        : 0;
      deltaWaitingOnYou += nowActionableCount - wasActionableCount;

      const stillWaitingOnYou = p.waitingOnYou && nowActionableCount > 0;
      return {
        ...p,
        decisions: patched,
        waitingOnYou: stillWaitingOnYou,
      };
    });

    if (changed) {
      this.pendingPrompts = nextPrompts;
      if (deltaWaitingOnYou !== 0) {
        this.updateCounts({
          ...this.counts,
          waitingOnYou: Math.max(0, this.counts.waitingOnYou + deltaWaitingOnYou),
        });
        if (this.filteredCounts !== null) {
          this.filteredCounts = {
            ...this.filteredCounts,
            waitingOnYou: Math.max(0, this.filteredCounts.waitingOnYou + deltaWaitingOnYou),
          };
        }
      }
    }
  }

  private scheduleLiveRefresh(): void {
    if (this.liveRefreshTimer !== null) {
      clearTimeout(this.liveRefreshTimer);
    }
    this.liveRefreshTimer = setTimeout(() => {
      this.liveRefreshTimer = null;
      if (!this.isConnected || !this.enabled || this.hubNotFound) return;
      if (!this.error && !this.pendingSearchTimeout) {
        if (
          this.pendingNeedsReset ||
          (this.pendingEffectiveSort() === 'project' && this.pendingPagedBeyondFirst)
        ) {
          this.pendingNextCursor = '';
          this.pendingPagedBeyondFirst = false;
          if (this.activeTab === 'pending') {
            void this.loadPendingPage({ mode: 'reset' });
          } else {
            void this.loadPendingPage({ mode: 'live' });
          }
        } else {
          void this.loadPendingPage({ mode: 'live' });
        }
      }
      if (this.activeTab === 'made') {
        if (
          !this.hasDateRangeError &&
          !this.madeSearchTimeout &&
          !this.madeNeedsReset &&
          (this.madeLoaded || this.madeLoading)
        ) {
          void this.loadMadePage({ mode: 'live' });
        }
      } else if (this.madeLoaded || this.madeLoading) {
        this.madeStale = true;
      }
    }, 60);
  }

  private flushDebouncedSearchIfPending(): boolean {
    if (this.searchDebounceTimer === null) return false;
    clearTimeout(this.searchDebounceTimer);
    this.searchDebounceTimer = null;
    const nextQ = normalizeDecisionsSearchQuery(this.searchDraft);
    if (nextQ !== this.filterQ) {
      this.filterQ = nextQ;
      return true;
    }
    return false;
  }

  private invalidateInFlightMadeLoad(): void {
    this.madeNeedsReset = true;
    this.madeSearchTimeout = false;
    this.madeRefreshDeferred = false;
    if (this.madeAbort || this.madeLoading || this.madeLoadingMore) {
      ++this.madeGen;
      this.madeAbort?.abort();
      this.madeAbort = null;
      this.madeLoading = false;
      this.madeLoadingMore = false;
    }
  }

  private invalidateInFlightPendingLoad(): void {
    this.pendingNeedsReset = true;
    this.pendingSearchTimeout = false;
    this.pendingRefreshDeferred = false;
    if (this.pendingAbort || this.pendingResetInFlight || this.pendingLoadingMore) {
      ++this.pendingGen;
      this.pendingAbort?.abort();
      this.pendingAbort = null;
      this.pendingResetInFlight = false;
      this.pendingLoading = false;
      this.pendingLoadingMore = false;
    }
  }

  private switchTab(next: 'pending' | 'made'): void {
    if (next !== 'pending' && next !== 'made') return;
    if (this.activeTab === next) return;

    const flushedSearch = this.flushDebouncedSearchIfPending();
    if (flushedSearch) {
      if (next === 'pending') {
        this.invalidateInFlightMadeLoad();
        this.pendingNeedsReset = true;
      } else {
        this.invalidateInFlightPendingLoad();
        this.madeNeedsReset = true;
      }
    }

    if (next === 'pending') {
      const hadMadeOnlyFilters = Boolean(
        this.filterUser || this.filterProvenance || this.filterFrom || this.filterTo
      );
      if (hadMadeOnlyFilters) {
        this.filterUser = '';
        this.filterProvenance = '';
        this.filterFrom = '';
        this.filterTo = '';
        this.invalidateInFlightMadeLoad();
      }
    }

    const nextSort =
      next === 'pending'
        ? (this.pendingSortOverride ?? defaultProfileDecisionsSort('pending'))
        : (this.madeSortOverride ?? defaultProfileDecisionsSort('made'));

    this.activeTab = next;
    this.sort = nextSort;
    this.syncUrlState();

    const group = this.shadowRoot?.querySelector('sl-tab-group') as
      | (HTMLElement & { show?: (panel: string) => void })
      | null;
    if (typeof group?.show === 'function') {
      group.show(next);
    }

    if (next === 'pending') {
      if (this.pendingNeedsReset) {
        this.pendingNextCursor = '';
        this.pendingPagedBeyondFirst = false;
        void this.loadPendingPage({ mode: 'reset' });
      }
      return;
    }

    if (!this.hasDateRangeError) {
      if (this.madeNeedsReset) {
        this.madeNextCursor = '';
        this.madePagedBeyondFirst = false;
        void this.loadMadePage({ mode: 'reset' });
      } else if (!this.madeLoading) {
        if (!this.madeLoaded) {
          void this.loadMadePage({ mode: 'reset' });
        } else if (this.madeStale) {
          this.madeStale = false;
          void this.loadMadePage({ mode: 'live' });
        }
      }
    }
  }

  private setSort(next: ProfileDecisionsSort): void {
    if (next !== 'project' && next !== 'recent') return;
    if (this.sort === next) return;

    this.flushDebouncedSearchIfPending();
    this.sort = next;
    if (this.activeTab === 'pending') {
      this.pendingSortOverride = next !== defaultProfileDecisionsSort('pending') ? next : null;
      this.syncUrlState();
      this.pendingNextCursor = '';
      this.pendingPagedBeyondFirst = false;
      void this.loadPendingPage({ mode: 'reset' });
    } else {
      this.madeSortOverride = next !== defaultProfileDecisionsSort('made') ? next : null;
      this.syncUrlState();
      this.madeNextCursor = '';
      this.madePagedBeyondFirst = false;
      if (!this.hasDateRangeError) {
        void this.loadMadePage({ mode: 'reset' });
      }
    }
  }

  private onSortChange(e: Event): void {
    const val = (e.currentTarget as HTMLElement & { value?: string })?.value;
    if (val === 'project' || val === 'recent') {
      this.setSort(val);
    }
  }

  /** "See all in <project>" drill-down on Made · By project (R5, R-3). */
  private drillDownMadeProject(projectId: string): void {
    const cleaned = cleanProfileProjectValue(projectId);
    if (!cleaned) return;
    this.flushDebouncedSearchIfPending();
    const projectChanged = cleaned !== this.filterProject;
    this.filterProject = cleaned;
    this.sort = 'recent';
    this.madeSortOverride = null;
    if (projectChanged) {
      this.invalidateInFlightPendingLoad();
    }
    this.madeNextCursor = '';
    this.madePagedBeyondFirst = false;
    this.madeRefreshDeferred = false;
    this.syncUrlState();
    if (!this.hasDateRangeError) {
      void this.loadMadePage({ mode: 'reset' });
    }
  }

  private onTabShow(e: Event): void {
    if (e.target !== e.currentTarget) return;
    const name = (e as CustomEvent<{ name?: string }>).detail?.name;
    if (name === 'pending' || name === 'made') {
      this.switchTab(name);
    }
  }

  private onPromptChoiceAnswered(
    prompt: ProjectDecisionPromptView,
    e: CustomEvent<ChoiceAnsweredDetail>
  ): void {
    this.applyIncomingDecisionPatch(
      e.detail.messageId || prompt.promptMessageId,
      e.detail.decisions
    );
    this.scheduleLiveRefresh();
  }

  private onPromptMessageJump(
    prompt: ProjectDecisionPromptView,
    e: CustomEvent<{ messageId?: string }>
  ): void {
    if (!prompt.conversationAvailable) return;
    const targetMsgId = e.detail?.messageId?.trim();
    const projectId = prompt.project?.id || '';
    if (!targetMsgId || !projectId) return;
    const base = chatConversationPath({
      conversationKey: prompt.chatKey,
      projectId,
    });
    if (!base) return;
    navigateTo(`${base}#msg-${encodeURIComponent(targetMsgId)}`);
  }

  private onSearchInput(detail: DecisionsSearchInputDetail): void {
    this.searchDraft = detail.value;
    if (this.searchDebounceTimer !== null) {
      clearTimeout(this.searchDebounceTimer);
      this.searchDebounceTimer = null;
    }

    const commitSearch = (): void => {
      const nextQ = normalizeDecisionsSearchQuery(this.searchDraft);
      if (nextQ === this.filterQ && !this.pendingSearchTimeout && !this.madeSearchTimeout) {
        return;
      }
      this.filterQ = nextQ;
      this.onFiltersUpdated({ sharedChanged: true });
    };

    if (detail.immediate) {
      commitSearch();
      return;
    }
    this.searchDebounceTimer = setTimeout(() => {
      this.searchDebounceTimer = null;
      commitSearch();
    }, SEARCH_DEBOUNCE_MS);
  }

  private applyFilterPatch(patch: DecisionsFilterPatch): void {
    let sharedChanged = false;
    let madeOnlyChanged = false;

    if (patch.q !== undefined) {
      if (this.searchDebounceTimer !== null) {
        clearTimeout(this.searchDebounceTimer);
        this.searchDebounceTimer = null;
      }
      const nextQ = normalizeDecisionsSearchQuery(patch.q);
      this.searchDraft = nextQ;
      if (nextQ !== this.filterQ) {
        this.filterQ = nextQ;
        sharedChanged = true;
      }
    }
    if (patch.project !== undefined) {
      const nextProject = cleanProfileProjectValue(patch.project);
      if (nextProject !== this.filterProject) {
        this.filterProject = nextProject;
        sharedChanged = true;
      }
    }
    if (patch.conversation !== undefined && patch.conversation !== this.filterConversation) {
      this.filterConversation = patch.conversation;
      sharedChanged = true;
    }
    if (patch.agent !== undefined && patch.agent !== this.filterAgent) {
      this.filterAgent = patch.agent;
      sharedChanged = true;
    }
    if (patch.user !== undefined && patch.user !== this.filterUser) {
      this.filterUser = patch.user;
      madeOnlyChanged = true;
    }
    if (patch.provenance !== undefined && patch.provenance !== this.filterProvenance) {
      this.filterProvenance = patch.provenance;
      madeOnlyChanged = true;
    }
    if (patch.from !== undefined && patch.from !== this.filterFrom) {
      this.filterFrom = patch.from;
      madeOnlyChanged = true;
    }
    if (patch.to !== undefined && patch.to !== this.filterTo) {
      this.filterTo = patch.to;
      madeOnlyChanged = true;
    }

    if (sharedChanged || madeOnlyChanged) {
      this.onFiltersUpdated({ sharedChanged });
    }
  }

  private onFiltersUpdated(opts: { sharedChanged: boolean }): void {
    this.syncUrlState();

    if (this.activeTab === 'pending') {
      if (opts.sharedChanged) {
        this.pendingNextCursor = '';
        this.pendingPagedBeyondFirst = false;
        if (
          this.madeLoaded ||
          this.madeSearchTimeout ||
          this.madeLoading ||
          this.madeLoadingMore ||
          this.madeAbort !== null
        ) {
          this.invalidateInFlightMadeLoad();
        }
        void this.loadPendingPage({ mode: 'reset' });
      }
      return;
    }

    if (opts.sharedChanged) {
      this.invalidateInFlightPendingLoad();
    }
    this.madeNextCursor = '';
    this.madePagedBeyondFirst = false;
    this.madeRefreshDeferred = false;
    if (this.hasDateRangeError) {
      ++this.madeGen;
      this.madeAbort?.abort();
      this.madeAbort = null;
      this.madeLoading = false;
      this.madeLoadingMore = false;
      this.madeError = null;
      this.madeSearchTimeout = false;
      return;
    }
    void this.loadMadePage({ mode: 'reset' });
  }

  private clearFilters(): void {
    if (this.searchDebounceTimer !== null) {
      clearTimeout(this.searchDebounceTimer);
      this.searchDebounceTimer = null;
    }
    const hadShared = Boolean(
      this.filterProject ||
      this.filterQ ||
      this.searchDraft ||
      this.filterConversation ||
      this.filterAgent
    );
    const hadMadeOnly = Boolean(
      this.filterUser || this.filterProvenance || this.filterFrom || this.filterTo
    );
    this.filterProject = '';
    this.filterQ = '';
    this.searchDraft = '';
    this.filterConversation = '';
    this.filterAgent = '';
    this.filterUser = '';
    this.filterProvenance = '';
    this.filterFrom = '';
    this.filterTo = '';
    if (!hadShared && !hadMadeOnly && !this.pendingSearchTimeout && !this.madeSearchTimeout) {
      return;
    }
    this.onFiltersUpdated({
      sharedChanged: hadShared || this.pendingSearchTimeout || this.madeSearchTimeout,
    });
  }

  private retryAll(): void {
    this.booted = false;
    this.facetsHydrated = false;
    void this.bootIfEnabled();
  }

  private promptAnswerability(p: ProjectDecisionPromptView): AskerAnswerability {
    if (p.canAnswer) return ANSWERABLE;
    return {
      state: 'blocked',
      reason: p.disabledReason || GENERIC_ANSWER_DENIED_REASON,
    };
  }

  private conversationDisplayLabel(chatKey: string, conversationLabel: string): string {
    if (chatKey.startsWith('dm:')) return 'your DM';
    return conversationLabel || 'chat';
  }

  private conversationHref(
    chatKey: string,
    available: boolean,
    projectId: string | undefined
  ): string | null {
    if (!available || !projectId) return null;
    return chatConversationPath({ conversationKey: chatKey, projectId });
  }

  private formatLogWhen(iso: string): string {
    const today = formatInstant(new Date().toISOString(), 'date');
    const rowDate = formatInstant(iso, 'date');
    if (today && rowDate === today) {
      return formatInstant(iso, 'time');
    }
    return formatInstant(iso, 'datetime');
  }

  private get projectFacetOptions(): FacetComboboxOption[] {
    return this.facetsProjects.map((p) => ({
      value: p.id,
      label: p.name || p.slug || p.id,
    }));
  }

  private get projectExtraTags(): DecisionsFilterTag[] {
    if (!this.filterProject) return [];
    const name = this.projectDisplayName(undefined, this.filterProject);
    return [{ key: 'project', label: `Project: ${name}` }];
  }

  /** D5: Conversation facet options show their project as the muted combobox suffix. */
  private get profileConversationFacets(): ProjectDecisionConversationView[] {
    return this.facetsConversations.map((c) => {
      const projName = c.projectId ? this.projectDisplayName(undefined, c.projectId) : '';
      if (!projName) return c;
      const hint = c.hint
        ? c.hint.includes(projName)
          ? c.hint
          : `${projName} · ${c.hint}`
        : projName;
      return {
        ...c,
        hint,
      };
    });
  }

  /** D5: Agent facet options show their project from `agents[].projectId` when present, falling back gracefully when absent. */
  private get profileAgentFacets(): ProjectDecisionActorView[] {
    return this.facetsAgents.map((a) => {
      const projName = a.projectId ? this.projectDisplayName(undefined, a.projectId) : '';
      if (!projName) return a;
      const hint = a.hint
        ? a.hint.includes(projName)
          ? a.hint
          : `${projName} · ${a.hint}`
        : projName;
      return {
        ...a,
        hint,
      };
    });
  }

  private countActionableInPrompt(p: ProjectDecisionPromptView): number {
    if (!p.waitingOnYou) return 0;
    const n = p.decisions.filter(
      (d) => !p.waitingReasons?.[d.rowId] && actionableReason(d, this.currentUserId) === ''
    ).length;
    return n > 0 ? n : Math.max(1, p.decisions.length);
  }

  /** Groups `waitingPrompts` by project preserving the Hub's `sort=project` rank order (R4). */
  private groupWaitingPromptsByProject(
    waitingPrompts: readonly ProjectDecisionPromptView[]
  ): PendingProjectGroup[] {
    const groups: PendingProjectGroup[] = [];
    const byProject = new Map<string, PendingProjectGroup>();
    for (const p of waitingPrompts) {
      const pid = p.project?.id || 'unknown';
      let group = byProject.get(pid);
      if (!group) {
        group = {
          project: p.project ?? { id: pid, name: pid, slug: pid },
          prompts: [],
          decisionCount: 0,
        };
        byProject.set(pid, group);
        groups.push(group);
      }
      group.prompts.push(p);
      group.decisionCount += this.countActionableInPrompt(p);
    }
    return groups;
  }

  private waitingOnOthersItems(): WaitingOnOthersItem[] {
    const items: WaitingOnOthersItem[] = [];
    for (const p of this.pendingPrompts) {
      for (const d of p.decisions) {
        const reason = p.waitingReasons?.[d.rowId];
        if (reason) {
          items.push({ prompt: p, decision: d, reason });
        } else if (
          !p.waitingOnYou &&
          d.status !== 'replaced' &&
          !(d.status === 'decided' && d.decision?.provenance !== 'agent_interpreted')
        ) {
          items.push({ prompt: p, decision: d, reason: 'other_user_confirm' });
        }
      }
    }
    return items;
  }

  private renderSortBar(): TemplateResult {
    return html`
      <div class="sort-bar" data-sort-bar>
        <sl-radio-group
          size="small"
          data-sort-switch
          aria-label="Sort decisions"
          .value=${this.sort}
          @sl-change=${(e: Event): void => this.onSortChange(e)}
        >
          <sl-radio-button
            value="project"
            data-sort-option="project"
            ?checked=${this.sort === 'project'}
            @click=${(): void => this.setSort('project')}
          >
            By project
          </sl-radio-button>
          <sl-radio-button
            value="recent"
            data-sort-option="recent"
            ?checked=${this.sort === 'recent'}
            @click=${(): void => this.setSort('recent')}
          >
            Most recent
          </sl-radio-button>
        </sl-radio-group>
      </div>
    `;
  }

  private renderFilterPanel(): TemplateResult {
    return html`
      <scion-decisions-filter-panel
        scope="profile"
        .supportsSearch=${this.supportsSearch}
        .tab=${this.activeTab}
        .q=${this.filterQ}
        .searchText=${this.searchDraft}
        .conversation=${this.filterConversation}
        .agent=${this.filterAgent}
        .user=${this.filterUser}
        .provenance=${this.filterProvenance}
        .from=${this.filterFrom}
        .to=${this.filterTo}
        .extraTags=${this.projectExtraTags}
        .currentUserId=${this.currentUserId}
        .conversations=${this.profileConversationFacets}
        .agents=${this.profileAgentFacets}
        .answerers=${this.facetsAnswerers}
        @decisions-search-input=${(e: CustomEvent<DecisionsSearchInputDetail>): void =>
          this.onSearchInput(e.detail)}
        @decisions-filter-change=${(e: CustomEvent<DecisionsFilterPatch>): void =>
          this.applyFilterPatch(e.detail)}
        @decisions-filters-clear=${(): void => this.clearFilters()}
      >
        <scion-facet-combobox
          slot="leading-controls"
          data-filter="project"
          label="Project"
          placeholder="Any"
          .options=${this.projectFacetOptions}
          .value=${this.filterProject}
          @facet-change=${(e: CustomEvent<FacetChangeDetail>): void =>
            this.applyFilterPatch({ project: e.detail.value })}
        ></scion-facet-combobox>
      </scion-decisions-filter-panel>
    `;
  }

  private renderTruncatedBanner(): TemplateResult | typeof nothing {
    if (!this.truncatedProjects) return nothing;
    return html`
      <sl-alert variant="primary" open data-truncated-projects>
        <sl-icon slot="icon" name="info-circle"></sl-icon>
        Showing your 200 most recently active projects; pick a project to see others
      </sl-alert>
    `;
  }

  private renderSearchTimeoutAlert(): TemplateResult {
    return html`
      <sl-alert variant="warning" open data-search-timeout>
        <sl-icon slot="icon" name="hourglass-split"></sl-icon>
        Search took too long; narrow it with a project, conversation, agent, or date filter.
      </sl-alert>
    `;
  }

  private renderNoMatchState(tab: 'pending' | 'made'): TemplateResult {
    return html`
      <div class="empty" data-no-match=${tab}>
        <sl-icon name="search"></sl-icon>
        <p><b>No decisions match these filters</b></p>
        <p>
          <sl-button
            size="small"
            variant="text"
            data-clear-filters
            @click=${(): void => this.clearFilters()}
          >
            Clear filters
          </sl-button>
        </p>
      </div>
    `;
  }

  private renderWaitingTabBadge(): TemplateResult | typeof nothing {
    if (
      this.activeTab === 'pending' &&
      this.isPendingFiltered &&
      this.filteredCounts !== null &&
      (this.counts.waitingOnYou > 0 || this.filteredCounts.waitingOnYou > 0)
    ) {
      return html`<sl-badge variant="primary" pill data-waiting-badge
        >${this.filteredCounts.waitingOnYou} of ${this.counts.waitingOnYou}</sl-badge
      >`;
    }
    if (this.counts.waitingOnYou > 0) {
      return html`<sl-badge variant="primary" pill data-waiting-badge
        >${this.counts.waitingOnYou}</sl-badge
      >`;
    }
    return nothing;
  }

  override render(): TemplateResult {
    if (!this.enabled || this.hubNotFound) {
      return html`<scion-page-404 .pageData=${this.pageData}></scion-page-404>`;
    }

    if (this.initialLoading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading decisions...</p>
        </div>
      `;
    }

    if (this.error) {
      return html`
        <div class="error-state">
          <sl-icon name="exclamation-triangle"></sl-icon>
          <h2>Failed to Load Decisions</h2>
          <div class="error-details">${this.error}</div>
          <sl-button variant="primary" data-retry-pending @click=${(): void => this.retryAll()}>
            <sl-icon slot="prefix" name="arrow-clockwise"></sl-icon>
            Retry
          </sl-button>
        </div>
      `;
    }

    return html`
      <div class="ph">
        <h1>Decisions</h1>
      </div>
      <p class="sub">Decisions agents asked in conversations you can read, across your projects.</p>

      ${this.renderTruncatedBanner()}

      <sl-tab-group @sl-tab-show=${(e: Event): void => this.onTabShow(e)}>
        <sl-tab
          slot="nav"
          panel="pending"
          data-tab="pending"
          ?active=${this.activeTab === 'pending'}
          @click=${(): void => this.switchTab('pending')}
        >
          Waiting on you ${this.renderWaitingTabBadge()}
        </sl-tab>
        <sl-tab
          slot="nav"
          panel="made"
          data-tab="made"
          ?active=${this.activeTab === 'made'}
          @click=${(): void => this.switchTab('made')}
        >
          Made
        </sl-tab>

        <sl-tab-panel name="pending" ?active=${this.activeTab === 'pending'}>
          ${this.activeTab === 'pending'
            ? html`${this.renderSortBar()}${this.renderFilterPanel()}`
            : nothing}
          ${this.renderPendingPanel()}
        </sl-tab-panel>
        <sl-tab-panel name="made" ?active=${this.activeTab === 'made'}>
          ${this.activeTab === 'made'
            ? html`${this.renderSortBar()}${this.renderFilterPanel()}`
            : nothing}
          ${this.renderMadePanel()}
        </sl-tab-panel>
      </sl-tab-group>
    `;
  }

  private renderPendingPanel(): TemplateResult {
    if (this.pendingLoading) {
      return html`
        <div class="loading-state">
          <sl-spinner></sl-spinner>
          <p>Loading decisions...</p>
        </div>
      `;
    }

    if (this.pendingSearchTimeout) {
      return this.renderSearchTimeoutAlert();
    }

    const waitingPrompts = this.pendingPrompts.filter((p) => p.waitingOnYou);
    const othersItems = this.waitingOnOthersItems();
    const isFiltered = this.isPendingFiltered;
    const othersSummary =
      isFiltered && this.filteredCounts !== null
        ? `Waiting on others (${this.filteredCounts.waitingOnOthers} of ${this.counts.waitingOnOthers})`
        : `Waiting on others (${Math.max(this.counts.waitingOnOthers, othersItems.length)})`;
    const showOthersSection =
      othersItems.length > 0 ||
      (isFiltered &&
        this.filteredCounts !== null &&
        waitingPrompts.length > 0 &&
        this.counts.waitingOnOthers > 0);

    return html`
      ${waitingPrompts.length === 0
        ? isFiltered
          ? this.renderNoMatchState('pending')
          : html`
              <div class="empty" data-empty="pending">
                <sl-icon name="check2-circle"></sl-icon>
                <p><b>Nothing is waiting on you</b></p>
                <p>
                  When an agent asks you to choose across your projects, it shows up here.
                  <a
                    class="linkish"
                    href="?tab=made&sort=recent"
                    data-see-made
                    @click=${(e: Event): void => {
                      e.preventDefault();
                      this.switchTab('made');
                    }}
                    >See decisions made</a
                  >
                </p>
              </div>
            `
        : this.sort === 'project'
          ? repeat(
              this.groupWaitingPromptsByProject(waitingPrompts),
              (g) => g.project.id,
              (g) => this.renderPendingProjectGroup(g)
            )
          : repeat(
              waitingPrompts,
              (p) => p.promptMessageId,
              (p) => this.renderPromptCard(p, true)
            )}
      ${this.pendingNextCursor
        ? html`
            <div class="more">
              <sl-button
                size="small"
                data-load-more="pending"
                ?loading=${this.pendingLoadingMore}
                ?disabled=${this.pendingLoadingMore}
                @click=${(): void => void this.loadPendingPage({ mode: 'append' })}
              >
                Load more
              </sl-button>
            </div>
          `
        : nothing}
      ${showOthersSection
        ? html`
            <sl-details class="others" data-waiting-others summary=${othersSummary}>
              <div class="mlist">
                ${repeat(
                  othersItems,
                  (item) => item.decision.rowId,
                  (item) => this.renderWaitingOnOthersItem(item)
                )}
              </div>
            </sl-details>
          `
        : nothing}
    `;
  }

  private renderPendingProjectGroup(group: PendingProjectGroup): TemplateResult {
    const projectName = this.projectDisplayName(group.project);
    const projectHref = group.project.id
      ? `/projects/${encodeURIComponent(group.project.id)}/decisions`
      : null;

    return html`
      <section class="pending-project-group" data-pending-group=${group.project.id}>
        <div class="groupsec">
          <h3>${projectName}</h3>
          <sl-badge variant="neutral" pill>${group.decisionCount}</sl-badge>
          ${projectHref
            ? html`<a class="linkish right" data-open-project-decisions href=${projectHref}
                >Open project Decisions</a
              >`
            : nothing}
        </div>
        ${repeat(
          group.prompts,
          (p) => p.promptMessageId,
          (p) => this.renderPromptCard(p, false)
        )}
      </section>
    `;
  }

  private renderPromptCard(p: ProjectDecisionPromptView, showProjectBadge = false): TemplateResult {
    const projectId = p.project?.id || '';
    const projectName = showProjectBadge ? this.projectDisplayName(p.project) : '';
    const whereLabel = this.conversationDisplayLabel(p.chatKey, p.conversationLabel);
    const convHref = this.conversationHref(p.chatKey, p.conversationAvailable, projectId);
    const permalink =
      p.promptAvailable && convHref
        ? `${convHref}#msg-${encodeURIComponent(p.promptMessageId)}`
        : null;
    const mode = p.answerMode === 'set' || p.answerMode === 'single' ? p.answerMode : undefined;

    return html`
      <div
        class="pcard"
        data-prompt-card=${p.promptMessageId}
        @choice-answered=${(e: CustomEvent<ChoiceAnsweredDetail>): void =>
          this.onPromptChoiceAnswered(p, e)}
        @scroll-to-message=${(e: CustomEvent<{ messageId?: string }>): void =>
          this.onPromptMessageJump(p, e)}
        @choice-jump=${(e: CustomEvent<{ messageId?: string }>): void =>
          this.onPromptMessageJump(p, e)}
      >
        <div class="pcard-head">
          ${projectName
            ? html`<sl-badge variant="neutral" data-card-project>${projectName}</sl-badge>`
            : nothing}
          <span
            class="avatar"
            aria-hidden="true"
            style="background: ${avatarColor(p.askerAgentId || p.askerAgentName)}"
            >${avatarInitial(p.askerAgentName)}</span
          >
          <b>${p.askerAgentName}</b>
          <span>
            in
            ${convHref
              ? html`<a class="linkish" data-conv-link href=${convHref}>${whereLabel}</a>`
              : html`<span class="muted" data-conv-unavailable>${whereLabel}</span>`}
          </span>
          <span class="muted" title=${formatInstantWithZone(p.createdAt)}
            >· ${formatRelative(p.createdAt, { style: 'short' })}</span
          >
          <span class="right">
            ${permalink
              ? html`<a class="linkish" data-open-in-chat href=${permalink}
                  ><sl-icon name="chat-left-text" aria-hidden="true"></sl-icon> Open in chat</a
                >`
              : !p.promptAvailable
                ? html`<span class="purged-note" data-purged-note
                    ><sl-icon name="archive" aria-hidden="true"></sl-icon>
                    <span>Original message no longer available</span></span
                  >`
                : nothing}
          </span>
        </div>
        <scion-chat-choices
          .messageId=${p.promptMessageId}
          .decisions=${p.decisions}
          .currentUserId=${this.currentUserId}
          .answerMode=${mode}
          .answerability=${this.promptAnswerability(p)}
          surface="decisions_page"
        ></scion-chat-choices>
      </div>
    `;
  }

  private renderWaitingOnOthersItem(item: WaitingOnOthersItem): TemplateResult {
    const { prompt: p, decision: d, reason } = item;
    const projectId = p.project?.id || '';
    const projectName = this.projectDisplayName(p.project);
    const whereLabel = this.conversationDisplayLabel(p.chatKey, p.conversationLabel);
    const convHref = this.conversationHref(p.chatKey, p.conversationAvailable, projectId);
    const permalink =
      p.promptAvailable && convHref
        ? `${convHref}#msg-${encodeURIComponent(p.promptMessageId)}`
        : null;

    let reasonBadge: TemplateResult;
    if (reason === 'other_user_confirm') {
      const owner = d.decision?.answeredByName || 'Another user';
      reasonBadge = html`<span class="badge interp"
        ><sl-icon name="robot" aria-hidden="true"></sl-icon>${owner} to confirm</span
      >`;
    } else if (reason === 'read_only') {
      reasonBadge = html`<span class="badge">read-only for you</span>`;
    } else if (reason === 'message_denied') {
      reasonBadge = html`<span class="badge">messaging restricted</span>`;
    } else {
      reasonBadge = html`<span class="badge">not answerable by you</span>`;
    }

    return html`
      <div class="item" data-others-row=${d.rowId}>
        <div class="top">
          ${d.key ? html`<span class="key">${d.key}</span>` : nothing}
          <b>${d.question}</b>
          ${reasonBadge}
        </div>
        <div class="meta">
          ${projectName ? html`${projectName} · ` : nothing}${p.askerAgentName} · ${whereLabel} ·
          ${formatRelative(p.createdAt, { style: 'short' })}
          ${permalink
            ? html` · <a class="linkish" href=${permalink}>Open in chat</a>`
            : !p.promptAvailable
              ? html` · Original message no longer available`
              : nothing}
        </div>
      </div>
    `;
  }

  private renderMadePanel(): TemplateResult {
    const isFiltered = this.isMadeFiltered;
    const isByProject = this.sort === 'project';
    const isEmpty = isByProject ? this.madeGroups.length === 0 : this.madeEntries.length === 0;

    return html`
      ${this.hasDateRangeError
        ? html`
            <div class="date-range-error" role="alert" data-date-range-error>
              <sl-icon name="exclamation-triangle" aria-hidden="true"></sl-icon>
              <span>Start date must be on or before end date.</span>
            </div>
          `
        : nothing}
      ${this.madeLoading
        ? html`
            <div class="loading-state">
              <sl-spinner></sl-spinner>
              <p>Loading decision log...</p>
            </div>
          `
        : this.madeSearchTimeout
          ? this.renderSearchTimeoutAlert()
          : this.madeError
            ? html`
                <div class="error-state">
                  <sl-icon name="exclamation-triangle"></sl-icon>
                  <h2>Failed to Load Decision Log</h2>
                  <div class="error-details">${this.madeError}</div>
                  <sl-button
                    variant="primary"
                    data-retry-made
                    @click=${(): void => void this.loadMadePage({ mode: 'reset' })}
                  >
                    Retry
                  </sl-button>
                </div>
              `
            : isEmpty
              ? isFiltered
                ? this.renderNoMatchState('made')
                : html`
                    <div class="empty" data-empty="made">
                      <sl-icon name="journal-text"></sl-icon>
                      <p><b>No decisions recorded yet</b></p>
                      <p>Answers to agents' decisions across your projects appear here.</p>
                    </div>
                  `
              : isByProject
                ? repeat(
                    this.madeGroups,
                    (g) => g.project.id,
                    (g) => this.renderMadeProjectGroup(g)
                  )
                : html`
                    <table class="log" data-made-table>
                      <thead>
                        <tr>
                          <th>When</th>
                          <th>Project</th>
                          <th>Decision</th>
                          <th>Question</th>
                          <th>Answer</th>
                          <th>Answered by</th>
                          <th>Provenance</th>
                          <th>Surface</th>
                          <th>Agent · where</th>
                        </tr>
                      </thead>
                      <tbody>
                        ${repeat(
                          this.madeEntries,
                          (e) => e.answerId,
                          (e) => this.renderMadeRow(e)
                        )}
                      </tbody>
                    </table>

                    <div class="mlist made-mobile-list" data-made-mobile-list>
                      ${repeat(
                        this.madeEntries,
                        (e) => e.answerId,
                        (e) => this.renderMadeItem(e, true)
                      )}
                    </div>

                    ${this.madeNextCursor
                      ? html`
                          <div class="more">
                            <sl-button
                              size="small"
                              data-load-more="made"
                              ?loading=${this.madeLoadingMore}
                              ?disabled=${this.madeLoadingMore}
                              @click=${(): void => void this.loadMadePage({ mode: 'append' })}
                            >
                              Load more
                            </sl-button>
                          </div>
                        `
                      : isFiltered
                        ? html`<div class="more">
                            <span class="muted" style="font-size: 0.8rem"
                              >End of matching decisions</span
                            >
                          </div>`
                        : nothing}
                  `}
    `;
  }

  private renderMadeProjectGroup(g: ProfileDecisionMadeGroupView): TemplateResult {
    const projectName = this.projectDisplayName(g.project);
    const drillSearch = buildProfileDecisionsUrlSearch({
      tab: 'made',
      sort: 'recent',
      project: g.project.id,
      q: this.filterQ,
      conversation: this.filterConversation,
      agent: this.filterAgent,
      user: this.filterUser,
      provenance: this.filterProvenance,
      from: this.filterFrom,
      to: this.filterTo,
    });

    return html`
      <section class="made-project-group" data-made-group=${g.project.id}>
        <div class="groupsec">
          <h3>${projectName}</h3>
          ${g.project.id
            ? html`<a
                class="linkish right"
                href=${`?${drillSearch}`}
                data-see-all-project=${g.project.id}
                data-has-more=${g.more ? 'true' : 'false'}
                @click=${(e: Event): void => {
                  e.preventDefault();
                  this.drillDownMadeProject(g.project.id);
                }}
                >See all in ${projectName}</a
              >`
            : nothing}
        </div>
        <div class="mlist">
          ${repeat(
            g.entries,
            (e) => e.answerId,
            (e) => this.renderMadeItem(e, false, g.project)
          )}
        </div>
      </section>
    `;
  }

  private renderAnsweredBy(e: ProjectDecisionLogEntryView): TemplateResult | string {
    if (this.currentUserId && e.answeredByUserId === this.currentUserId) {
      return 'you';
    }
    if (!e.answeredByUserId) {
      return html`${e.answeredByName || 'Unknown'} <span class="muted">(deleted user)</span>`;
    }
    return e.answeredByName || 'Unknown';
  }

  private renderProvenanceBadge(e: ProjectDecisionLogEntryView): TemplateResult {
    if (e.provenance === 'agent_interpreted') {
      return html`<span class="badge interp"
        ><sl-icon name="robot" aria-hidden="true"></sl-icon>interpreted</span
      >`;
    }
    if (e.provenance === 'direct' && e.outcome === 'confirm') {
      return html`<span class="badge conf"
        ><sl-icon name="patch-check" aria-hidden="true"></sl-icon>confirmed</span
      >`;
    }
    if (e.provenance === 'direct' && e.outcome === 'override') {
      return html`<span class="badge over"
        ><sl-icon name="arrow-left-right" aria-hidden="true"></sl-icon>changed</span
      >`;
    }
    return html`<span class="badge">direct</span>`;
  }

  private replyHrefForEntry(
    e: ProjectDecisionLogEntryView,
    projectId: string | undefined
  ): string | null {
    if (!e.sourceMessageId || !e.sourceMessageAvailable || !e.conversationAvailable) {
      return null;
    }
    const convHref = this.conversationHref(e.chatKey, e.conversationAvailable, projectId);
    if (!convHref) return null;
    return `${convHref}#msg-${encodeURIComponent(e.sourceMessageId)}`;
  }

  private renderMadeRow(e: ProjectDecisionLogEntryView): TemplateResult {
    const isFollow = e.outcome === 'confirm' || e.outcome === 'override';
    const surf = surfaceDisplay(e.surface);
    const projectId = e.project?.id || '';
    const projectName = this.projectDisplayName(e.project);
    const projectHref = projectId ? `/projects/${encodeURIComponent(projectId)}/decisions` : null;
    const whereLabel = this.conversationDisplayLabel(e.chatKey, e.conversationLabel);
    const convHref = this.conversationHref(e.chatKey, e.conversationAvailable, projectId);
    const replyHref = this.replyHrefForEntry(e, projectId);

    return html`
      <tr class=${isFollow ? 'follow' : ''} data-answer-row=${e.answerId}>
        <td>
          <time datetime=${e.at} title=${formatInstantWithZone(e.at)}
            >${this.formatLogWhen(e.at)}</time
          >
        </td>
        <td data-project-cell>
          ${projectHref
            ? html`<a class="linkish" data-project-link href=${projectHref}>${projectName}</a>`
            : projectName}
        </td>
        <td><span class="key">${e.decision.key || e.decision.ref}</span> v${e.decision.version}</td>
        <td class="q">
          <span class="q-text" title=${e.decision.question}>${e.decision.question}</span>
        </td>
        <td><b>${e.label || e.choiceId}</b></td>
        <td data-answered-by>${this.renderAnsweredBy(e)}</td>
        <td data-provenance>
          ${this.renderProvenanceBadge(e)}
          ${e.provenance === 'agent_interpreted' && replyHref
            ? html` <a class="linkish" data-reply-link href=${replyHref}>reply</a>`
            : e.provenance === 'direct' && e.outcome === 'override' && e.previousChoiceLabel
              ? html` <span class="muted">from ${e.previousChoiceLabel}</span>`
              : e.provenance === 'direct' && e.outcome === 'answer' && e.matchedDefault
                ? html` <span class="muted">recommended</span>`
                : nothing}
        </td>
        <td data-surface>
          <span class="surf"
            >${surf.icon
              ? html`<sl-icon name=${surf.icon} aria-hidden="true"></sl-icon>`
              : nothing}${surf.label}</span
          >
        </td>
        <td data-agent-where>
          ${e.decision.askerAgentName} ·
          ${convHref
            ? html`<a class="linkish" data-conv-link href=${convHref}>${whereLabel}</a>`
            : html`<span class="muted" data-conv-unavailable>${whereLabel}</span>`}
        </td>
      </tr>
    `;
  }

  private renderMadeItem(
    e: ProjectDecisionLogEntryView,
    includeProjectInMeta: boolean,
    fallbackProject?: ProjectDecisionProjectRef
  ): TemplateResult {
    const surf = surfaceDisplay(e.surface);
    const proj = e.project?.id ? e.project : fallbackProject;
    const projectId = proj?.id || '';
    const projectName = this.projectDisplayName(proj);
    const whereLabel = this.conversationDisplayLabel(e.chatKey, e.conversationLabel);
    const convHref = this.conversationHref(e.chatKey, e.conversationAvailable, projectId);
    const replyHref = this.replyHrefForEntry(e, projectId);

    return html`
      <div class="item" data-mobile-answer=${e.answerId}>
        <div class="top">
          <span class="key">${e.decision.key || e.decision.ref}</span>
          <b>${e.label || e.choiceId}</b>
          ${this.renderProvenanceBadge(e)}
        </div>
        <div>${e.decision.question}</div>
        <div class="meta">
          ${includeProjectInMeta && projectName
            ? html`${projectName} · `
            : nothing}${this.renderAnsweredBy(e)}
          ${e.outcome === 'override' && e.previousChoiceLabel
            ? html` · from ${e.previousChoiceLabel}`
            : nothing}
          · ${surf.label} · ${e.decision.askerAgentName} ·
          ${convHref
            ? html`<a class="linkish" href=${convHref}>${whereLabel}</a>`
            : html`<span class="muted">${whereLabel}</span>`}
          ·
          <time datetime=${e.at} title=${formatInstantWithZone(e.at)}
            >${this.formatLogWhen(e.at)}</time
          >
          ${replyHref ? html` · <a class="linkish" href=${replyHref}>reply</a>` : nothing}
        </div>
      </div>
    `;
  }
}

declare global {
  interface HTMLElementTagNameMap {
    'scion-page-profile-decisions': ScionPageProfileDecisions;
  }
}
