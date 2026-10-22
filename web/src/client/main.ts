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
 * Client entry point
 *
 * Handles client-side routing and real-time state management via SSE.
 */

import themeCSS from '../styles/theme.css?inline';
import '@shoelace-style/shoelace/dist/themes/light.css';
import '@shoelace-style/shoelace/dist/themes/dark.css';

import type { AuthMeResponse, PageData, User } from '../shared/types.js';
import { stateManager } from './state.js';
import { setDocumentTitle } from './page-title.js';
import { readInitialPageData, setInitialPageData, takeInitialPageData } from './ssr-page-data.js';
import { configureReadinessMarks, readinessNavigationStarted } from './readiness-marks.js';
import { CHAT_DM_ROUTE, CHAT_SPACE_ROUTE, CHAT_THREAD_ROUTE } from './chat-routes.js';
import { chatNotifications } from './chat-notifications.js';
import { chatUnread, startChatUnreadIfEligible } from './chat-unread.js';
import type { TerminalCoordinator } from './terminal-coordinator.js';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import type { TerminalWorkspacePersistence } from './terminal-persistence.js';
import type { TerminalResources, TerminalSession } from './terminal-sessions.js';
import {
  TERMINAL_PALETTE_NEW_AGENT_EVENT,
  type TerminalPaletteNewAgentDetail,
} from './terminal-workspace-events.js';
// The terminal workspace is a lazy chunk (ptone/scion#5306): only type
// imports of its modules here; values come from loadTerminalWorkspace().
import {
  loadTerminalWorkspace,
  type TerminalWorkspaceModule,
} from './terminal-workspace-loader.js';
import { showToast } from '../utils/toast.js';
import { FLAT_BROKERS_FLAG, isFeatureEnabled } from '../utils/feature-flags.js';
import { resolveBrokerRoute } from './broker-routes.js';
import { applyServerFeatureFlags } from './server-feature-flags.js';
import { setPreferredTimeZone } from '../utils/time.js';
import { withTimeout } from './with-timeout.js';
import {
  type AdminStatus,
  hasAnyPermission,
  ROUTE_PERMISSION_MAP,
  SUPERADMIN_ROUTES,
} from '../lib/admin-permissions.js';
import { ACCOUNT_TEARDOWN_EVENT, type AccountTeardownDetail } from '../utils/auth.js';
import { clearAdminStatus, loadAdminStatus } from './admin-status.js';
import { chatRecentFiles } from './chat-recent-files.js';
import { isInPagePop, pushRouteEntry, type RouteShell } from './route-history.js';
import { browserPath, stripBasePath } from './navigation.js';
import { clearChatScrollAnchor } from '../components/shared/chat/chat-scroll-anchor.js';
import { deepActiveElement } from '../components/shared/deep-active-element.js';
import { installViewportFrame } from './viewport.js';
import {
  buildRecentFilesScope,
  shouldClearRecentFilesOnTeardown,
} from './chat-recent-files-lifecycle.js';

/**
 * Milliseconds `init()` waits for the SSR-path display-timezone refresh
 * before proceeding with the first render in Auto and letting
 * `DisplayZoneController` correct it late (review R3-1).
 */
const TZ_LOAD_BUDGET_MS = 1500;

// Inject theme CSS so it loads regardless of whether the page is served by
// the Vite dev server (index.html) or the Go SPA shell template (web.go).
{
  const style = document.createElement('style');
  style.textContent = themeCSS;
  document.head.appendChild(style);
}

// Import Shoelace base path config (needed for icons).
// Icons are copied to public/shoelace/ by scripts/copy-shoelace-icons.mjs
// so they are available under both the Vite dev server and the Go server.
import { setBasePath } from '@shoelace-style/shoelace/dist/utilities/base-path.js';
setBasePath('/shoelace');

// Explicitly import all Shoelace components used in the app. This is the
// only place Shoelace components are registered: the SPA shell loads no CDN
// autoloader (it could not see sl-* elements inside LitElement shadow roots
// anyway, and loaded a second Shoelace copy for light-DOM ones). Every sl-*
// tag used in src/ must be imported here; shoelace-registration.test.ts
// enforces it.
import '@shoelace-style/shoelace/dist/components/badge/badge.js';
import '@shoelace-style/shoelace/dist/components/breadcrumb/breadcrumb.js';
import '@shoelace-style/shoelace/dist/components/breadcrumb-item/breadcrumb-item.js';
import '@shoelace-style/shoelace/dist/components/button/button.js';
import '@shoelace-style/shoelace/dist/components/button-group/button-group.js';
import '@shoelace-style/shoelace/dist/components/card/card.js';
import '@shoelace-style/shoelace/dist/components/checkbox/checkbox.js';
import '@shoelace-style/shoelace/dist/components/drawer/drawer.js';
import '@shoelace-style/shoelace/dist/components/icon/icon.js';
import '@shoelace-style/shoelace/dist/components/icon-button/icon-button.js';
import '@shoelace-style/shoelace/dist/components/input/input.js';
import '@shoelace-style/shoelace/dist/components/option/option.js';
import '@shoelace-style/shoelace/dist/components/select/select.js';
import '@shoelace-style/shoelace/dist/components/spinner/spinner.js';
import '@shoelace-style/shoelace/dist/components/progress-bar/progress-bar.js';
import '@shoelace-style/shoelace/dist/components/textarea/textarea.js';
import '@shoelace-style/shoelace/dist/components/tooltip/tooltip.js';
import '@shoelace-style/shoelace/dist/components/dialog/dialog.js';
import '@shoelace-style/shoelace/dist/components/divider/divider.js';
import '@shoelace-style/shoelace/dist/components/dropdown/dropdown.js';
import '@shoelace-style/shoelace/dist/components/menu/menu.js';
import '@shoelace-style/shoelace/dist/components/menu-item/menu-item.js';
import '@shoelace-style/shoelace/dist/components/menu-label/menu-label.js';
import '@shoelace-style/shoelace/dist/components/alert/alert.js';
import '@shoelace-style/shoelace/dist/components/radio-group/radio-group.js';
import '@shoelace-style/shoelace/dist/components/radio-button/radio-button.js';
import '@shoelace-style/shoelace/dist/components/radio/radio.js';
import '@shoelace-style/shoelace/dist/components/range/range.js';
import '@shoelace-style/shoelace/dist/components/switch/switch.js';
import '@shoelace-style/shoelace/dist/components/details/details.js';
import '@shoelace-style/shoelace/dist/components/tab-group/tab-group.js';
import '@shoelace-style/shoelace/dist/components/tab/tab.js';
import '@shoelace-style/shoelace/dist/components/tab-panel/tab-panel.js';
import '@shoelace-style/shoelace/dist/components/tag/tag.js';

// Import app shell and core shared components (always needed)
import '../components/app-shell.js';
import '../components/shared/nav.js';
import '../components/shared/header.js';
import '../components/shared/breadcrumb.js';
import '../components/shared/status-badge.js';

// Profile shell (lazy-loaded with profile routes)
// import '../components/profile/profile-shell.js';
// import '../components/profile/profile-nav.js';

// Page components are lazy-loaded per route — see ROUTES below.

/** Current authenticated user, fetched once on init */
let currentUser: User | null = null;

/**
 * The admin-status flags the route guard last fetched fresh on entering an
 * admin page (see client/admin-status.ts). Used to allow hub-admin users
 * (not just super-admins) into admin pages, with the permissions array for
 * per-route permission checks.
 */
let cachedAdminStatus: AdminStatus | null = null;
let terminalCoordinator: TerminalCoordinator | null = null;
let terminalWorkspace: TerminalWorkspaceRoot | null = null;
let terminalPersistence: TerminalWorkspacePersistence | null = null;
/** The loaded terminal workspace chunk; set before the coordinator is created. */
let terminalModule: TerminalWorkspaceModule | null = null;
/** Set after account teardown to prevent stale callbacks from recreating sessions. */
let accountTornDown = false;
let routeOutlet: HTMLElement | null = null;
const terminalNavigations = new Map<string, number>();
/** The agent in the most recent /terminals/<agentId> route, for a move to this window. */
let terminalRouteAgentId: string | null = null;
/** True while a move to this window is running. */
let terminalMoveRunning = false;
const uuidPath = '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}';
const terminalAgentRoute = new RegExp(`^/terminals/(${uuidPath})$`, 'i');
/** The pre-workspace terminal URL; any agent id is redirected to /terminals/<id>. */
const legacyTerminalRoute = /^\/agents\/([^/]+)\/terminal$/;

function ensureRoots(): HTMLElement | null {
  const app = document.getElementById('app');
  if (!app) return null;
  if (!routeOutlet || routeOutlet.parentElement !== app) {
    routeOutlet = document.createElement('div');
    routeOutlet.id = 'route-outlet';
    routeOutlet.style.cssText = 'height:100%;min-height:0';
    app.replaceChildren(routeOutlet);
  }
  return routeOutlet;
}

/**
 * The "open in another window" state (ptone/scion#4324): one status text
 * with its "?" help, and the move button under it.
 */
function showTerminalsOpenElsewhere(): void {
  if (!terminalModule) return;
  terminalWorkspace?.setStatus(terminalModule.TERMINALS_OPEN_ELSEWHERE_STATUS, {
    label: terminalModule.TERMINALS_OPEN_ELSEWHERE_HELP_LABEL,
    text: terminalModule.TERMINALS_OPEN_ELSEWHERE_HELP,
  });
  offerTerminalMove();
}

/** Shows "Move terminals to this window" under the non-owner status message. */
function offerTerminalMove(): void {
  if (!terminalModule) return;
  terminalWorkspace?.setStatusAction({
    label: terminalModule.MOVE_TERMINALS_LABEL,
    onClick: () => void runTerminalMove(),
  });
}

/**
 * Moves the terminals from the window that owns them to this one
 * (ptone/scion#3328): this window opens its own streams first, and the
 * owner closes its streams only after that. See moveTerminalsHere.
 */
async function runTerminalMove(): Promise<void> {
  const coordinator = terminalCoordinator;
  const persistence = terminalPersistence;
  const workspace = terminalWorkspace;
  const t = terminalModule;
  if (!coordinator || !persistence || !workspace || !t || terminalMoveRunning) return;
  terminalMoveRunning = true;
  try {
    const result = await t.moveTerminalsWithStatus({
      coordinator,
      persistence,
      workspace,
      ui: workspace,
      preferredAgentId: terminalRouteAgentId,
      retry: () => void runTerminalMove(),
    });
    if (result.status !== 'moved' || coordinator !== terminalCoordinator || accountTornDown) return;
    const path = result.agentId ? `/terminals/${result.agentId}` : '/terminals';
    const current = stripBasePath(window.location.pathname);
    if (!window.location.search && (current === '/terminals' || terminalAgentRoute.test(current))) {
      window.history.replaceState(window.history.state, '', browserPath(path));
      workspace.setCurrentPath(path);
      terminalRouteAgentId = result.agentId;
    }
  } finally {
    terminalMoveRunning = false;
  }
}

/** Delay before the first-load placeholder appears, so a cached chunk does not flash it. */
const TERMINAL_LOADING_DELAY_MS = 150;
/**
 * The first-load terminal placeholder: its timer, its element once shown,
 * and the navigation that currently owns it.
 */
interface TerminalLoadingState {
  owner: number;
  timer: ReturnType<typeof setTimeout>;
  el: HTMLElement | null;
}
let terminalLoading: TerminalLoadingState | null = null;

/** Removes the terminal loading placeholder (or cancels its timer), whoever owns it. */
function clearTerminalLoading(): void {
  if (!terminalLoading) return;
  clearTimeout(terminalLoading.timer);
  terminalLoading.el?.remove();
  terminalLoading = null;
}

/**
 * Shows "Loading terminals…" in #app after TERMINAL_LOADING_DELAY_MS, while
 * the terminal workspace chunk loads for the first time (the route outlet,
 * which holds the whole app shell, is hidden meanwhile). A newer first-load
 * terminal navigation adopts a placeholder that is already pending or shown,
 * so it neither flashes blank nor restarts the delay. Returns the cleanup for
 * navigation `owner`: it removes the placeholder only while `owner` still
 * owns it, so a superseded load cannot remove a newer navigation's. Non-
 * terminal navigations and an account teardown use clearTerminalLoading.
 */
function startTerminalLoading(app: HTMLElement, owner: number): () => void {
  if (terminalLoading) {
    terminalLoading.owner = owner;
  } else {
    const state: TerminalLoadingState = {
      owner,
      el: null,
      timer: setTimeout(() => {
        const el = document.createElement('div');
        el.id = 'terminal-workspace-loading';
        el.setAttribute('role', 'status');
        el.setAttribute('aria-live', 'polite');
        el.style.cssText =
          'height:var(--scion-app-height, 100dvh);display:flex;flex-direction:column;' +
          'align-items:center;justify-content:center;gap:var(--sl-spacing-small);' +
          'background:var(--scion-surface);color:var(--scion-text-muted)';
        const spinner = document.createElement('sl-spinner');
        spinner.style.fontSize = '2rem';
        const label = document.createElement('span');
        el.append(spinner, label);
        app.appendChild(el);
        state.el = el;
        // Fill the live region in a later task than its insertion: some
        // screen readers do not announce text a status node is inserted with.
        state.timer = setTimeout(() => {
          label.textContent = 'Loading terminals…';
        }, 0);
      }, TERMINAL_LOADING_DELAY_MS),
    };
    terminalLoading = state;
  }
  return (): void => {
    if (terminalLoading?.owner === owner) clearTerminalLoading();
  };
}

/**
 * Returns the terminal coordinator, creating it (and the workspace) on the
 * first /terminals render. The first call loads the terminal workspace chunk;
 * if navigation `thisNav` is superseded while it loads, nothing is created or
 * mounted and null is returned. A chunk load failure rejects.
 */
async function ensureTerminalCoordinator(thisNav: number): Promise<TerminalCoordinator | null> {
  if (!currentUser?.id || accountTornDown) return null;
  if (terminalCoordinator) return terminalCoordinator;
  const t = await loadTerminalWorkspace();
  // Same guard as the rest of renderRoute: a newer navigation (or an account
  // teardown) during the load wins, and this one must not mount anything.
  if (thisNav !== navigationId) return null;
  if (!currentUser?.id || accountTornDown) return null;
  if (terminalCoordinator) return terminalCoordinator;
  const app = document.getElementById('app');
  if (!app) return null;
  terminalModule = t;
  terminalWorkspace = new t.TerminalWorkspaceRoot(currentUser);
  app.appendChild(terminalWorkspace.element);
  terminalCoordinator = new t.TerminalCoordinator(
    {
      hubUrl: new URL(import.meta.env.BASE_URL, window.location.origin).href,
      accountId: currentUser.id,
    },
    {
      initialize: (): Promise<TerminalResources> =>
        Promise.reject(new Error('Retained pane initializer required.')),
      create: (registry, agentId, options): TerminalSession =>
        terminalWorkspace!.create(registry, agentId, options),
      select: (session, signal, requestId): void => {
        if (signal.aborted) throw new Error('Terminal workspace stopped.');
        const expected = requestId && terminalNavigations.get(requestId);
        if (requestId) terminalNavigations.delete(requestId);
        if (expected !== undefined && expected !== navigationId)
          throw new Error('Navigation superseded.');
        terminalWorkspace!.select(session);
        if (expected === undefined) navigateTo(`/terminals/${session.state.agentId}`);
      },
    }
  );
  terminalPersistence = new t.TerminalWorkspacePersistence({
    coordinator: terminalCoordinator,
    workspace: terminalWorkspace,
    onRestoredSelection: (agentId): void => {
      // Only while the route is still bare /terminals: restore() can settle
      // after the user has already navigated elsewhere.
      if (window.location.pathname !== browserPath('/terminals') || window.location.search) return;
      window.history.replaceState(window.history.state, '', browserPath(`/terminals/${agentId}`));
      terminalWorkspace!.setCurrentPath(`/terminals/${agentId}`);
    },
  });
  // After a move to another window, this window shows the "owned elsewhere"
  // state, with the button to move the terminals back.
  terminalCoordinator.onRelinquished(() => showTerminalsOpenElsewhere());
  // "Jump to agent" palette, new agent in a multi-pane layout only (the
  // workspace places an already-open agent itself, and navigates like a rail
  // click when only one pane is on screen — see its selectFromPalette).
  terminalWorkspace.element.addEventListener(TERMINAL_PALETTE_NEW_AGENT_EVENT, (e) => {
    const { agentId } = (e as CustomEvent<TerminalPaletteNewAgentDetail>).detail;
    if (!terminalCoordinator || !terminalWorkspace) return;
    void t.openPalettePickedAgent({
      coordinator: terminalCoordinator,
      workspace: terminalWorkspace,
      agentId,
      navigations: terminalNavigations,
      navigationId,
      currentNavigationId: () => navigationId,
      notify: (message) => showToast(message, 'neutral'),
    });
  });
  return terminalCoordinator;
}

/**
 * Fetch the current authenticated user from the backend session.
 * Returns null if not authenticated.
 */
async function fetchCurrentUser(): Promise<User | null> {
  try {
    const res = await fetch('/auth/me', { credentials: 'include' });
    if (!res.ok) return null;
    const data = (await res.json()) as AuthMeResponse;
    return {
      id: data.id,
      email: data.email,
      name: data.displayName || data.name || '',
      avatar: data.avatarUrl || data.avatar,
      role: data.role || undefined,
      preferences: data.preferences || undefined,
      testIdentity: data.testIdentity?.expiresAt
        ? { expiresAt: data.testIdentity.expiresAt }
        : undefined,
    };
  } catch {
    return null;
  }
}

/**
 * Refreshes the display-timezone preference and applies it to the
 * effective-zone store (`setPreferredTimeZone`). Used when the SSR-injected
 * user (`prefetchPageData`, `pkg/hub/web.go`) already supplied `currentUser`
 * without `preferences` — that field is deliberately never cached on the
 * session (`webSessionUser.Preferences`) and so is absent from SSR data,
 * only ever populated by a live `/auth/me` read.
 *
 * Callers should bound the wait with `withTimeout` (review R3-1): this
 * fetch is cosmetic, but blocking first render on it unconditionally means
 * a stalled `/auth/me` (a busy store, a stuck proxy) blocks the whole shell
 * until the browser's own fetch timeout, which can be minutes. A timed-out
 * wait still lets this promise keep running in the background — when it
 * lands, `setPreferredTimeZone` fires `DISPLAY_TIMEZONE_CHANGED_EVENT`, and
 * every mounted `DisplayZoneController` subscriber re-renders in the
 * correct zone.
 *
 * That self-correction is **not universal** (review R4-1): it only helps a
 * component that re-renders cleanly from a fresh formatter call. A
 * component that *also* caches a wall-clock string derived from the zone
 * (e.g. a `datetime-local` input pre-populated via `toWallClockInput`) needs
 * its own re-derivation logic on top of the controller — see
 * `access-boundary-schedule-editor.ts`'s `willUpdate` and `time.ts`'s
 * "Effective-zone store" header — or a late arrival silently moves an
 * untouched field's stored instant. A bounded wait is still strictly better
 * than an unbounded one on every surface; it just isn't a complete fix by
 * itself for every surface.
 */
async function loadPreferredTimeZone(): Promise<void> {
  try {
    const res = await fetch('/auth/me', { credentials: 'include' });
    if (!res.ok) return;
    const data = (await res.json()) as AuthMeResponse;
    setPreferredTimeZone(data.preferences?.timezone);
  } catch {
    // Non-critical — the effective zone falls back to the browser zone.
  }
}

/**
 * Route configuration mapping URL patterns to page component tag names.
 * Each route includes a lazy loader that dynamically imports the page module,
 * which registers its custom element as a side effect.
 */
interface RouteConfig {
  pattern: RegExp;
  tag: string;
  load: () => Promise<unknown>;
}

const ROUTES: RouteConfig[] = [
  {
    pattern: /^\/login$/,
    tag: 'scion-login-page',
    load: () => import('../components/pages/login.js'),
  },
  {
    pattern: /^\/invite$/,
    tag: 'scion-page-invite',
    load: () => import('../components/pages/invite.js'),
  },
  {
    pattern: /^\/onboarding$/,
    tag: 'scion-page-onboarding',
    load: () => import('../components/pages/onboarding.js'),
  },
  { pattern: /^\/$/, tag: 'scion-page-home', load: () => import('../components/pages/home.js') },
  {
    pattern: /^\/projects$/,
    tag: 'scion-page-projects',
    load: () => import('../components/pages/projects.js'),
  },
  {
    pattern: /^\/agents$/,
    tag: 'scion-page-agents',
    load: () => import('../components/pages/agents.js'),
  },
  // /brokers and /brokers/{id} are resolved by resolveBrokerRoute
  // (broker-routes.ts), ahead of this table.
  {
    pattern: /^\/skills$/,
    tag: 'scion-page-skills',
    load: () => import('../components/pages/skills.js'),
  },
  {
    pattern: /^\/skills\/new$/,
    tag: 'scion-page-skill-create',
    load: () => import('../components/pages/skill-create.js'),
  },
  {
    pattern: /^\/skills\/[^/]+$/,
    tag: 'scion-page-skill-detail',
    load: () => import('../components/pages/skill-detail.js'),
  },
  {
    pattern: /^\/admin\/skill-registries$/,
    tag: 'scion-page-admin-skill-registries',
    load: () => import('../components/pages/admin-skill-registries.js'),
  },
  {
    pattern: /^\/admin\/skill-registries\/[^/]+$/,
    tag: 'scion-page-admin-skill-registry-detail',
    load: () => import('../components/pages/admin-skill-registry-detail.js'),
  },
  {
    pattern: /^\/admin\/scheduler$/,
    tag: 'scion-page-admin-scheduler',
    load: () => import('../components/pages/admin-scheduler.js'),
  },
  {
    pattern: /^\/admin\/users$/,
    tag: 'scion-page-admin-users',
    load: () => import('../components/pages/admin-users.js'),
  },
  {
    pattern: /^\/admin\/roles$/,
    tag: 'scion-page-admin-roles',
    load: () => import('../components/pages/admin-roles.js'),
  },
  {
    pattern: /^\/admin\/role-bindings$/,
    tag: 'scion-page-admin-role-bindings',
    load: () => import('../components/pages/admin-role-bindings.js'),
  },
  {
    pattern: /^\/admin\/roles\/[^/]+$/,
    tag: 'scion-page-admin-role-detail',
    load: () => import('../components/pages/admin-role-detail.js'),
  },
  {
    pattern: /^\/admin\/access-boundaries$/,
    tag: 'scion-page-admin-access-boundaries',
    load: () => import('../components/pages/admin-access-boundaries.js'),
  },
  {
    pattern: /^\/admin\/access-boundaries\/new$/,
    tag: 'scion-page-admin-access-boundary-editor',
    load: () => import('../components/pages/admin-access-boundary-editor.js'),
  },
  {
    pattern: /^\/admin\/access-boundaries\/[^/]+$/,
    tag: 'scion-page-admin-access-boundary-detail',
    load: () => import('../components/pages/admin-access-boundary-detail.js'),
  },
  {
    pattern: /^\/admin\/access-boundaries\/[^/]+\/edit$/,
    tag: 'scion-page-admin-access-boundary-editor',
    load: () => import('../components/pages/admin-access-boundary-editor.js'),
  },
  {
    pattern: /^\/admin\/groups$/,
    tag: 'scion-page-admin-groups',
    load: () => import('../components/pages/admin-groups.js'),
  },
  {
    pattern: /^\/admin\/quotas$/,
    tag: 'scion-page-admin-quotas',
    load: () => import('../components/pages/admin-quotas.js'),
  },
  {
    pattern: /^\/admin\/groups\/[^/]+$/,
    tag: 'scion-page-admin-group-detail',
    load: () => import('../components/pages/admin-group-detail.js'),
  },
  {
    pattern: /^\/health$/,
    tag: 'scion-page-health-dashboard',
    load: () => import('../components/pages/health-dashboard.js'),
  },
  {
    pattern: /^\/admin\/health$/,
    tag: 'scion-page-health-dashboard',
    load: () => import('../components/pages/health-dashboard.js'),
  },
  {
    pattern: /^\/metrics$/,
    tag: 'scion-page-metrics',
    load: () => import('../components/pages/metrics-dashboard.js'),
  },
  {
    pattern: /^\/admin\/metrics$/,
    tag: 'scion-page-metrics',
    load: () => import('../components/pages/metrics-dashboard.js'),
  },
  {
    pattern: /^\/admin\/diagnostics$/,
    tag: 'scion-page-diagnostics',
    load: () => import('../components/pages/diagnostics.js'),
  },
  {
    pattern: /^\/admin\/maintenance$/,
    tag: 'scion-page-admin-maintenance',
    load: () => import('../components/pages/admin-maintenance.js'),
  },
  {
    pattern: /^\/admin\/integrations$/,
    tag: 'scion-page-admin-integrations',
    load: () => import('../components/pages/admin-integrations.js'),
  },
  {
    pattern: /^\/admin\/integrations\/[^/]+$/,
    tag: 'scion-page-admin-integrations',
    load: () => import('../components/pages/admin-integrations.js'),
  },
  {
    pattern: /^\/admin\/server-config$/,
    tag: 'scion-page-admin-server-config',
    load: () => import('../components/pages/admin-server-config.js'),
  },
  {
    pattern: /^\/admin\/federation$/,
    tag: 'scion-page-admin-federation',
    load: () => import('../components/pages/admin-federation.js'),
  },
  {
    pattern: /^\/settings$/,
    tag: 'scion-page-settings',
    load: () => import('../components/pages/settings.js'),
  },
  {
    pattern: /^\/settings\/templates\/[^/]+$/,
    tag: 'scion-page-template-detail',
    load: () => import('../components/pages/template-detail.js'),
  },
  {
    pattern: /^\/settings\/harness-configs\/[^/]+$/,
    tag: 'scion-page-harness-config-detail',
    load: () => import('../components/pages/harness-config-detail.js'),
  },
  // Parentless service accounts only (hub and user scope). Project-scoped ones
  // are managed from their project's settings tab, which is the surface that
  // computes their capabilities; see the page component for why there is no
  // /projects/{id}/service-accounts/{id} twin.
  //
  // DELIBERATELY NOT IN ADMIN_ROUTES, same as the template and harness-config
  // detail pages below the /settings umbrella. scion-page-settings IS admin-
  // gated, so the tab that links here is admin-only in practice; a non-admin
  // reaching this URL directly gets a read-only page, because every button on
  // it is rendered from the server's per-account _capabilities and the Hub
  // already permits any logged-in user to READ a hub-scoped account
  // (hub-member-read-all; see handlers_gcp_identity.go getGCPServiceAccount).
  // Adding the tag here would deny a read the API grants, i.e. change who may
  // read hub-scoped accounts — which is an open policy question, not a routing
  // decision to settle here.
  {
    pattern: /^\/settings\/service-accounts\/[^/]+$/,
    tag: 'scion-page-gcp-service-account-detail',
    load: () => import('../components/pages/gcp-service-account-detail.js'),
  },
  {
    pattern: /^\/profile\/env$/,
    tag: 'scion-page-profile-env-vars',
    load: () => import('../components/pages/profile-env-vars.js'),
  },
  {
    pattern: /^\/profile\/secrets$/,
    tag: 'scion-page-profile-secrets',
    load: () => import('../components/pages/profile-secrets.js'),
  },
  {
    pattern: /^\/profile\/settings$/,
    tag: 'scion-page-profile-settings',
    load: () => import('../components/pages/profile-settings.js'),
  },
  {
    pattern: /^\/profile\/tokens$/,
    tag: 'scion-page-profile-tokens',
    load: () => import('../components/pages/profile-tokens.js'),
  },
  {
    pattern: /^\/profile\/telegram$/,
    tag: 'scion-page-profile-telegram',
    load: () => import('../components/pages/profile-telegram.js'),
  },
  {
    pattern: /^\/profile\/teams$/,
    tag: 'scion-page-profile-teams',
    load: () => import('../components/pages/profile-teams.js'),
  },
  {
    pattern: /^\/profile\/discord$/,
    tag: 'scion-page-profile-discord',
    load: () => import('../components/pages/profile-discord.js'),
  },
  {
    pattern: /^\/profile\/skills$/,
    tag: 'scion-page-profile-skills',
    load: () => import('../components/pages/profile-skills.js'),
  },
  {
    pattern: /^\/profile\/templates\/[^/]+$/,
    tag: 'scion-page-template-detail',
    load: () => import('../components/pages/template-detail.js'),
  },
  {
    pattern: /^\/profile\/templates$/,
    tag: 'scion-page-profile-templates',
    load: () => import('../components/pages/profile-templates.js'),
  },
  {
    pattern: /^\/profile$/,
    tag: 'scion-page-profile-env-vars',
    load: () => import('../components/pages/profile-env-vars.js'),
  },
  {
    pattern: /^\/github-app\/installed$/,
    tag: 'scion-page-github-app-setup',
    load: () => import('../components/pages/github-app-setup.js'),
  },
  {
    pattern: /^\/projects\/new$/,
    tag: 'scion-page-project-create',
    load: () => import('../components/pages/project-create.js'),
  },
  {
    pattern: /^\/projects\/[^/]+\/settings$/,
    tag: 'scion-page-project-settings',
    load: () => import('../components/pages/project-settings.js'),
  },
  {
    pattern: /^\/projects\/[^/]+\/templates\/[^/]+$/,
    tag: 'scion-page-template-detail',
    load: () => import('../components/pages/template-detail.js'),
  },
  {
    pattern: /^\/projects\/[^/]+\/harness-configs\/[^/]+$/,
    tag: 'scion-page-harness-config-detail',
    load: () => import('../components/pages/harness-config-detail.js'),
  },
  {
    // Artifacts list (experiment hub.artifacts; the page renders 404 when off).
    pattern: /^\/artifacts$/,
    tag: 'scion-page-artifacts',
    load: () => import('../components/pages/artifacts.js'),
  },
  {
    // Artifact page (experiment hub.artifacts; the page renders 404 when off).
    pattern: /^\/projects\/[^/]+\/artifacts\/[^/]+(?:\/v\/[1-9][0-9]*)?$/,
    tag: 'scion-page-artifact-detail',
    load: () => import('../components/pages/artifact-detail.js'),
  },
  {
    pattern: /^\/projects\/[^/]+\/schedules$/,
    tag: 'scion-page-project-schedules',
    load: () => import('../components/pages/project-schedules.js'),
  },
  {
    pattern: /^\/projects\/[^/]+\/metrics$/,
    tag: 'scion-page-project-metrics',
    load: () => import('../components/pages/metrics-dashboard.js'),
  },
  {
    pattern: /^\/projects\/[^/]+$/,
    tag: 'scion-page-project-detail',
    load: () => import('../components/pages/project-detail.js'),
  },
  {
    pattern: /^\/agents\/new$/,
    tag: 'scion-page-agent-create',
    load: () => import('../components/pages/agent-create.js'),
  },
  {
    pattern: /^\/agents\/graph$/,
    tag: 'scion-page-agent-graph',
    load: () => import('../components/pages/agent-graph.js'),
  },
  {
    pattern: /^\/agents\/[^/]+\/configure$/,
    tag: 'scion-page-agent-configure',
    load: () => import('../components/pages/agent-configure.js'),
  },
  {
    // Edit agent page (experiment web.agent_edit; the page renders 404 when off).
    pattern: /^\/agents\/[^/]+\/edit$/,
    tag: 'scion-page-agent-edit',
    load: () => import('../components/pages/agent-edit.js'),
  },
  {
    pattern: /^\/agents\/[^/]+$/,
    tag: 'scion-page-agent-detail',
    load: () => import('../components/pages/agent-detail.js'),
  },
  // Chat mode routes (Phase 5 — top-level chat)
  {
    pattern: /^\/chat$/,
    tag: 'scion-page-chat',
    load: () => import('../components/pages/chat.js'),
  },
  // Wave-2 v2 chat routes: space, thread, and DM navigation
  {
    pattern: CHAT_THREAD_ROUTE,
    tag: 'scion-page-chat',
    load: () => import('../components/pages/chat.js'),
  },
  {
    pattern: CHAT_SPACE_ROUTE,
    tag: 'scion-page-chat',
    load: () => import('../components/pages/chat.js'),
  },
  {
    pattern: CHAT_DM_ROUTE,
    tag: 'scion-page-chat',
    load: () => import('../components/pages/chat.js'),
  },
  // Readable deep-link: /chat/<project-slug>/<thread-id>
  {
    pattern: /^\/chat\/[^/]+\/[^/]+$/,
    tag: 'scion-page-chat',
    load: () => import('../components/pages/chat.js'),
  },
  // Wave-1 agent-based route OR project-slug space link (resolved in chat.ts)
  {
    pattern: /^\/chat\/[^/]+$/,
    tag: 'scion-page-chat',
    load: () => import('../components/pages/chat.js'),
  },
];

/**
 * Routes that render without the app shell (full-page layout)
 */
const STANDALONE_ROUTES = new Set([
  'scion-login-page',
  'scion-page-invite',
  'scion-page-onboarding',
]);

/**
 * Routes that render inside the profile shell instead of the main app shell
 */
const PROFILE_ROUTES = new Set([
  'scion-page-profile-env-vars',
  'scion-page-profile-secrets',
  'scion-page-profile-settings',
  'scion-page-profile-tokens',
  'scion-page-profile-telegram',
  'scion-page-profile-teams',
  'scion-page-profile-discord',
  'scion-page-profile-skills',
  'scion-page-profile-templates',
]);

/**
 * Routes that render inside the chat shell (Phase 5 — top-level chat mode).
 * NOT standalone (chat has chrome, just different chrome from app shell).
 */
const CHAT_ROUTES = new Set(['scion-page-chat']);

/**
 * Routes that require admin role. Non-admin users are redirected to dashboard.
 */
const ADMIN_ROUTES = new Set([
  'scion-page-settings',
  'scion-page-admin-scheduler',
  'scion-page-admin-maintenance',
  'scion-page-admin-users',
  'scion-page-admin-groups',
  'scion-page-admin-roles',
  'scion-page-admin-role-detail',
  'scion-page-admin-role-bindings',
  'scion-page-admin-access-boundaries',
  'scion-page-admin-access-boundary-detail',
  'scion-page-admin-access-boundary-editor',
  'scion-page-admin-group-detail',
  'scion-page-admin-quotas',
  'scion-page-admin-server-config',
  'scion-page-admin-federation',
  'scion-page-admin-integrations',
  'scion-page-admin-skill-registries',
  'scion-page-admin-skill-registry-detail',
  'scion-page-diagnostics',
  'scion-page-health-dashboard',
  'scion-page-metrics',
]);

// ---------------------------------------------------------------------------
// Global error boundary — registered once so all shells share it.
// Without this, unhandled errors in a long-lived surface (like chat mode)
// silently fail. See design.md Section 4.1.
// ---------------------------------------------------------------------------
window.addEventListener('error', (event) => {
  console.error('[Scion] Unhandled error:', {
    message: event.message,
    source: event.filename,
    lineno: event.lineno,
    colno: event.colno,
    error: event.error as unknown,
  });
});

/**
 * First-run redirect: if the system has not completed onboarding, replace the
 * URL with /onboarding before the first render. Started at the top of init()
 * and awaited with the other boot requests (ptone/scion#5307).
 */
async function redirectIfOnboardingIncomplete(): Promise<void> {
  const skipRedirectPaths = ['/onboarding', '/login', '/invite'];
  if (skipRedirectPaths.includes(stripBasePath(window.location.pathname))) return;
  try {
    const statusRes = await fetch('/api/v1/system/status', { credentials: 'include' });
    if (statusRes.ok) {
      const status = (await statusRes.json()) as { complete?: boolean };
      if (!status.complete) {
        sessionStorage.setItem('onboardingStatus', JSON.stringify(status));
        window.history.replaceState({}, '', browserPath('/onboarding'));
      }
    }
  } catch {
    // System status endpoint unavailable (non-workstation mode) — skip redirect
  }
}

window.addEventListener('unhandledrejection', (event) => {
  console.error('[Scion] Unhandled promise rejection:', event.reason);
});

/**
 * Initialize the client-side application
 */
async function init(): Promise<void> {
  console.info('[Scion] Initializing client...');

  // Size the app frame to the visible area while the on-screen keyboard is
  // open (iOS), and undo any page pan in frame mode. Installed for the life
  // of the page, so the disposer is not kept.
  installViewportFrame();

  // Get initial data from SSR
  const initialData = getInitialData();
  // Fixed for this document: a change of the setting applies on the next full load.
  configureReadinessMarks(initialData?.readinessMarks === true);
  if (initialData) {
    console.info('[Scion] Initial page data:', initialData.path);
    if (initialData.user) {
      currentUser = initialData.user;
    }
    // Keep the SSR payload for the first render only (see ssr-page-data.ts).
    setInitialPageData(initialData);
  }

  // Start the page chunk for the initial path now (ptone/scion#5307): the
  // import overlaps the boot requests below instead of following them. It
  // only fetches; renderRoute still gates the render (flags, permissions).
  preloadInitialRoute(stripBasePath(window.location.pathname));

  // Start the feature-flag and onboarding-status fetches now so they overlap
  // the auth and component work below; both are awaited, together with the
  // timezone refresh, before the first render.
  const featureFlagsReady = applyServerFeatureFlags();
  const onboardingChecked = redirectIfOnboardingIncomplete();

  // Fetch current user from session if not provided by SSR
  let tzReady: Promise<void> = Promise.resolve();
  if (!currentUser) {
    currentUser = await fetchCurrentUser();
    if (currentUser) {
      setPreferredTimeZone(currentUser.preferences?.timezone);
    }
  } else {
    // SSR supplied the user without `preferences` (never cached on the
    // session); refresh it from the live endpoint. Awaited below alongside
    // featureFlagsReady, not fire-and-forget (review R2-1): a chat thread
    // already in the DOM at first render would otherwise show at least its
    // first messages in the browser zone instead of the preference, with
    // nothing to correct it until some unrelated re-render (the
    // DISPLAY_TIMEZONE_CHANGED_EVENT a late-arriving preference dispatches
    // only helps a component that is listening for it, which a component
    // not yet mounted cannot be). Bounded to TZ_LOAD_BUDGET_MS (review
    // R3-1): a stalled `/auth/me` must not hang first render — past the
    // budget, render in Auto and let a late result correct itself via
    // DisplayZoneController once it lands (the fetch itself is not
    // cancelled, only the wait for it).
    tzReady = withTimeout(loadPreferredTimeZone(), TZ_LOAD_BUDGET_MS).then(() => undefined);
  }

  // Start the shared admin-status request now, without waiting for it: the
  // nav joins it when it renders (see client/admin-status.ts), so a cold
  // load sends one request instead of one here plus one per nav instance.
  if (currentUser?.id) {
    void loadAdminStatus(currentUser.id);
  }

  // DM messages and read-state changes are published on user.<id>.chat.*, so
  // the state manager must know who we are before it opens the first SSE
  // connection.
  if (currentUser?.id) {
    stateManager.setCurrentUserId(currentUser.id);
    // Chat message popups are driven off those events. Started here rather
    // than from the chat page because a DM has to reach you on any page.
    chatNotifications.start({
      id: currentUser.id,
      email: currentUser.email,
      name: currentUser.name,
    });
    // The recent-files index (native chat quick palette "Documents") is
    // scoped to this identity + hub/base path; initialize only now that the
    // user is known.
    chatRecentFiles.setScope(
      buildRecentFilesScope(currentUser, window.location.origin, import.meta.env.BASE_URL)
    );
  }

  // Wait for core shell components to be defined (page components are lazy-loaded)
  await Promise.all([
    customElements.whenDefined('scion-app'),
    customElements.whenDefined('scion-nav'),
    customElements.whenDefined('scion-header'),
    customElements.whenDefined('scion-breadcrumb'),
    customElements.whenDefined('scion-status-badge'),
  ]);

  console.info('[Scion] Components defined, setting up router...');

  // Render the initial page based on current URL (strip proxy prefix for route
  // matching). Feature flags must be settled first — renderRoute gates /chat on
  // them, and rendering early would flash a page the server has disabled.
  // The onboarding check must be settled too: it may replace the URL with
  // /onboarding. tzReady is awaited alongside them (review R2-1), bounded to
  // TZ_LOAD_BUDGET_MS (review R3-1); all three started above and overlap,
  // so this adds no latency beyond the slowest of them.
  await Promise.all([featureFlagsReady, onboardingChecked, tzReady]);
  ensureRoots();

  // The unread conversation count (the header's chat badge and the tab-title
  // badge) is unread state, not notification state: it runs for every
  // signed-in user regardless of the push preference, and on every page.
  // After the flags settle — with chat disabled the endpoint it reads is not
  // even registered.
  // On a chat first page the first refresh goes out now and the page shares
  // it; elsewhere it waits for idle (see startChatUnreadIfEligible).
  const initialPath = stripBasePath(window.location.pathname);
  startChatUnreadIfEligible(
    chatUnread,
    !!currentUser,
    isFeatureEnabled('web.native_chat'),
    CHAT_ROUTES.has(resolveRoute(initialPath).tag),
    currentUser?.id
  );

  // Setup client-side router for navigation
  setupRouter();
  // Include query string on initial render so terminal layout state
  // from a shared/bookmarked URL can be restored on page load (#1715).
  const initialSearch = window.location.search;
  await renderRoute(initialSearch ? `${initialPath}${initialSearch}` : initialPath);

  // Account teardown: dispose terminals on logout/auth-expiry before redirect.
  // The event fires synchronously from performLogout() or auth-expiry detection
  // so cross-tab teardown completes before the page navigates away.
  window.addEventListener(ACCOUNT_TEARDOWN_EVENT, (e) => {
    // A chat scroll position belongs to this account's session.
    clearChatScrollAnchor();
    // So does its admin status: drop it and any request still in flight.
    clearAdminStatus();
    // Explicit logout only: suspend ingestion and clear this account's
    // persisted key and memory before the logout POST runs, so nothing async
    // can race a response into a store that is no longer this identity's. An
    // auth-expiry teardown may resume the same account after re-auth, so it
    // does not clear recents.
    const reason = (e as CustomEvent<AccountTeardownDetail>).detail?.reason;
    if (shouldClearRecentFilesOnTeardown(reason)) {
      chatRecentFiles.clearForLogout();
    }

    clearTerminalLoading();
    if (accountTornDown) return;
    accountTornDown = true;
    try {
      terminalCoordinator?.teardownAccount();
    } catch (e) {
      console.error('[Teardown] session cleanup error:', e);
    } finally {
      terminalCoordinator = null;
      // Hide the workspace UI immediately so the login page does not show
      // hidden active terminals underneath.
      terminalWorkspace?.show(false);
      terminalWorkspace = null;
    }
  });

  // Disconnect SSE on page unload
  window.addEventListener('beforeunload', () => {
    stateManager.disconnect();
    terminalCoordinator?.stop();
  });

  console.info('[Scion] Client initialization complete');
}

/**
 * Retrieves initial page data from SSR-injected script tag
 */
function getInitialData(): PageData | null {
  return readInitialPageData(document);
}

/** Fallback route for unmatched paths */
const NOT_FOUND_ROUTE: RouteConfig = {
  pattern: /./,
  tag: 'scion-page-404',
  load: () => import('../components/pages/not-found.js'),
};

/**
 * Resolves a URL path to a route configuration
 */
function resolveRoute(path: string): RouteConfig {
  const brokerRoute = resolveBrokerRoute(path, isFeatureEnabled(FLAT_BROKERS_FLAG));
  if (brokerRoute) return brokerRoute;
  for (const route of ROUTES) {
    if (route.pattern.test(path)) {
      return route;
    }
  }
  return NOT_FOUND_ROUTE;
}

/**
 * Determines which shell type a route tag requires.
 */
type ShellType = 'standalone' | 'profile' | 'chat' | 'app';

function getShellType(tag: string): ShellType {
  if (STANDALONE_ROUTES.has(tag)) return 'standalone';
  if (PROFILE_ROUTES.has(tag)) return 'profile';
  if (CHAT_ROUTES.has(tag)) return 'chat';
  return 'app';
}

/**
 * Starts the imports a route renders with: the page module, plus the profile
 * or chat shell when that shell is not defined yet. Each import registers its
 * custom element as a side effect; repeated calls reuse the module.
 */
function startRouteLoads(route: RouteConfig, shellType: ShellType): Promise<unknown> {
  const loads: Promise<unknown>[] = [route.load()];
  if (shellType === 'profile' && !customElements.get('scion-profile-shell')) {
    loads.push(
      import('../components/profile/profile-shell.js'),
      import('../components/profile/profile-nav.js')
    );
  }
  if (shellType === 'chat' && !customElements.get('scion-chat-shell')) {
    loads.push(import('../components/chat/chat-shell.js'));
  }
  return Promise.all(loads);
}

/**
 * Fetches the initial path's page chunk while the boot requests are in
 * flight (ptone/scion#5307). Nothing is rendered: renderRoute starts the same
 * imports (the module loader dedupes them) and still applies the flag and
 * permission checks first. The terminal routes are left to their own lazy
 * loader. A failure here is ignored; renderRoute's own load reports it.
 */
function preloadInitialRoute(pathname: string): void {
  if (
    pathname === '/terminals' ||
    terminalAgentRoute.test(pathname) ||
    legacyTerminalRoute.test(pathname)
  ) {
    return;
  }
  const route = resolveRoute(pathname);
  startRouteLoads(route, getShellType(route.tag)).catch(() => undefined);
}

/** Cached shell element and its type, reused across navigations */
let activeShell: { type: ShellType; element: HTMLElement } | null = null;

/** Navigation counter to cancel stale renders when rapid navigations occur */
let navigationId = 0;

/**
 * Renders the page component for the given path into #app.
 * Lazily imports the page module before creating the element.
 * Reuses the shell element (sidebar, header, etc.) when possible
 * to avoid full-page redraws on navigation.
 */
async function renderRoute(path: string): Promise<void> {
  const appContainer = ensureRoots();
  if (!appContainer) return;
  const thisNav = ++navigationId;
  readinessNavigationStarted(thisNav);

  // The SSR payload belongs to this document and is offered to the first
  // render only, whichever route it is; it is cleared here whether or not it
  // matches, so a later client-side navigation never receives it.
  const initialData = takeInitialPageData(path, currentUser);

  // Strip query string and hash for route matching
  let pathname = path.split('?')[0].split('#')[0];
  const legacyAgent = pathname.match(legacyTerminalRoute)?.[1];
  if (legacyAgent) {
    pathname = `/terminals/${legacyAgent}`;
    path = pathname;
    window.history.replaceState({}, '', browserPath(path));
  }
  if (pathname === '/terminals' || terminalAgentRoute.test(pathname)) {
    if (!currentUser?.id) {
      navigateTo('/login');
      return;
    }
    // Hide the previous page before the workspace chunk loads, as before the
    // chunk was lazy: the URL already says /terminals, so the old page must
    // not stay interactive under it while the load runs. A newer navigation
    // un-hides it (and reuses the page, see returningFromTerminal below).
    // Hiding blurs the focused element; a failed load gives focus back. Look
    // through shadow roots: document.activeElement is only the outer host.
    const focusBeforeLoad = deepActiveElement();
    appContainer.hidden = true;
    // First load only: the outlet (with the whole app shell) is hidden and
    // the workspace does not exist yet, so show a loading state meanwhile.
    const app = document.getElementById('app');
    let stopLoading: (() => void) | null = null;
    if (!terminalCoordinator && app) stopLoading = startTerminalLoading(app, thisNav);
    else clearTerminalLoading();
    let coordinator: TerminalCoordinator | null;
    try {
      coordinator = await ensureTerminalCoordinator(thisNav);
    } catch (err) {
      // The chunk failed to load (for example after a deploy removed it).
      // Like a failed page chunk, the previous page comes back on screen;
      // the loader does not cache the failure, so the next visit retries.
      console.error('[Scion] Failed to load the terminal workspace:', err);
      if (thisNav === navigationId) {
        appContainer.hidden = false;
        // Only if nothing else took focus during the load.
        const now = deepActiveElement();
        if (
          (now === null || now === document.body) &&
          focusBeforeLoad instanceof HTMLElement &&
          focusBeforeLoad.isConnected
        ) {
          focusBeforeLoad.focus({ preventScroll: true });
        }
        showToast('Could not load terminals. Reload the page to try again.', 'danger');
      }
      return;
    } finally {
      // Success, failure or supersede: this load's placeholder goes.
      stopLoading?.();
    }
    if (thisNav !== navigationId) return;
    setDocumentTitle('Terminals');
    // No coordinator (account torn down, or no #app): nothing to show or open.
    // Otherwise terminalModule is set too: it is assigned before the
    // coordinator is created.
    const t = terminalModule;
    if (!coordinator || !t) return;
    terminalWorkspace?.setUser(currentUser);
    terminalWorkspace?.setCurrentPath(path);
    terminalWorkspace?.show(true);

    // ── URL layout restoration (#1715) ──────────────────────────────
    // Parse layout state from query params BEFORE default single-agent
    // selection to avoid a flash of single → multi layout transition.
    const queryString = path.includes('?') ? path.split('?')[1] : window.location.search;
    const layoutUrl = t.parseLayoutUrl(queryString);

    // ── Persisted terminal list restore (ptone/scion#2278) ──────────
    // Runs for every render into /terminals…, before the URL-driven code
    // below: an explicit URL decides what is visible and connected, and
    // the saved list decides rail membership only.
    if (terminalPersistence) {
      await terminalPersistence.restore(t.restoreUrlIntent(pathname, queryString));
      if (thisNav !== navigationId) return;
    }

    if (layoutUrl && terminalWorkspace) {
      // Suppress URL sync while restoring to avoid feedback loops
      terminalWorkspace.setSuppressUrlSync(true);
      try {
        // Restore the layout preset and open agents from URL slots.
        // Map agent IDs to session keys, opening new sessions as needed.
        const sessionKeys = await t.openLayoutSlots({
          coordinator,
          workspace: terminalWorkspace,
          slots: layoutUrl.slots,
          navigations: terminalNavigations,
          navigationId: thisNav,
          currentNavigationId: () => navigationId,
        });
        if (!sessionKeys) return;
        // Restore the layout manager state with the resolved session keys
        terminalWorkspace.layoutManager.restore(layoutUrl.preset, sessionKeys);
      } finally {
        terminalWorkspace.setSuppressUrlSync(false);
        // Sync URL once after restoration to ensure canonical form
        terminalWorkspace.syncUrlFromLayout();
      }
      return;
    }

    // Default behavior: single-agent from path
    const agentId = pathname.match(terminalAgentRoute)?.[1];
    terminalRouteAgentId = agentId?.toLowerCase() ?? null;
    if (agentId) {
      const requestId = coordinator.supported ? crypto.randomUUID() : undefined;
      if (requestId) terminalNavigations.set(requestId, thisNav);
      const result = await coordinator.open(agentId, requestId);
      if (requestId && result.status !== 'pending') terminalNavigations.delete(requestId);
      if (thisNav === navigationId && !coordinator.isOwner) {
        if (t.offersMove(result.status) && !terminalMoveRunning) {
          showTerminalsOpenElsewhere();
        } else {
          terminalWorkspace?.setStatus(t.nonOwnerOpenStatus(result.status));
          if (!terminalMoveRunning) terminalWorkspace?.setStatusAction(null);
        }
      }
    } else if (terminalPersistence && !terminalMoveRunning) {
      // A bare /terminals in a window that does not own the terminals
      // (for example a new window): show the same "open in another
      // window" state as the other non-owner screens, not the empty one.
      const elsewhere = await t.terminalsOpenElsewhere({
        coordinator,
        persistence: terminalPersistence,
      });
      if (elsewhere && thisNav === navigationId && !terminalMoveRunning) {
        showTerminalsOpenElsewhere();
      }
    }
    return;
  }
  // Detect whether we are returning from a terminal workspace view.
  // The terminal route handler hides the outlet without clearing it, so
  // the previous page and shell are still live in the DOM.
  const returningFromTerminal = appContainer.hidden;
  // Leaving /terminals during a first load: its placeholder goes now.
  clearTerminalLoading();
  appContainer.hidden = false;
  terminalWorkspace?.show(false);
  const route = resolveRoute(pathname);
  const tag = route.tag;

  // Build page data with current user context for page components.
  // Include SSR-prefetched data on the initial render (see
  // takeInitialPageData) so page components can skip redundant API fetches.
  const pageData: PageData = {
    path,
    title: 'Scion',
    user: currentUser || undefined,
    data: initialData,
  };

  // Block /chat routes when the native_chat feature flag is disabled (O2).
  // Checked before the imports start, so a disabled chat is never fetched
  // here. (Chat and admin routes are disjoint.)
  if (CHAT_ROUTES.has(tag) && !isFeatureEnabled('web.native_chat')) {
    navigateTo('/');
    return;
  }

  const shellType = getShellType(tag);

  // Lazy-load the page component module (and profile/chat shell if needed).
  // Started before the admin-status request below so the chunk downloads
  // while it is in flight (ptone/scion#5307); nothing renders until both are
  // done and the permission check has passed.
  const loadsReady = startRouteLoads(route, shellType);

  // Block non-admin users from admin-only routes.
  // Hub-admin users (who have admin role bindings but not super-admin role)
  // are allowed through, alongside super-admins.
  //
  // Re-fetch admin status on every admin-route navigation so that role
  // grants or revocations made mid-session take effect immediately rather
  // than being cached for the entire SPA lifetime. The fresh result also
  // replaces the shared value (client/admin-status.ts); a nav already on
  // screen read that value once for this user, so only later nav renders
  // see it.
  //
  // Per-route permission checks: super-admin-only routes (Diagnostics,
  // Maintenance) require isSuperAdmin; other admin routes require at least
  // one matching permission from ROUTE_PERMISSION_MAP.
  if (ADMIN_ROUTES.has(tag)) {
    // A failed import is reported by the await below; a denied user is
    // redirected first and never reaches it.
    loadsReady.catch(() => undefined);
    cachedAdminStatus = await loadAdminStatus(currentUser?.id, { fresh: true });

    if (SUPERADMIN_ROUTES.has(tag)) {
      if (!cachedAdminStatus?.isSuperAdmin) {
        navigateTo('/');
        return;
      }
    } else {
      const requiredPerms = ROUTE_PERMISSION_MAP[tag];
      if (!requiredPerms || !hasAnyPermission(cachedAdminStatus, requiredPerms)) {
        navigateTo('/');
        return;
      }
    }
  }

  await loadsReady;

  // If another navigation started while we were loading, abort this render
  if (thisNav !== navigationId) return;

  // If the shell type changed, tear down and rebuild
  if (activeShell && activeShell.type !== shellType) {
    appContainer.innerHTML = '';
    activeShell = null;
  }

  if (shellType === 'standalone') {
    // Standalone pages render without a persistent shell
    appContainer.innerHTML = '';
    activeShell = null;
    const page = document.createElement(tag);
    appContainer.appendChild(page);
    setDocumentTitle(
      tag === 'scion-login-page'
        ? 'Login'
        : tag === 'scion-page-invite'
          ? 'Invite'
          : 'Page Not Found'
    );
  } else if (activeShell) {
    // Reuse existing shell — just update properties and swap page content
    const shell = activeShell.element as HTMLElement & {
      currentPath: string;
      user: User | null;
    };

    // When returning from a terminal workspace view to the same page that
    // was hidden (not destroyed), preserve the existing page element so
    // user state — drafts, selected conversation, scroll position — survives
    // the round trip.  The outlet was hidden (`appContainer.hidden = true`)
    // by the terminal route handler; the page inside is still live.
    // Only skip the swap when the tag matches AND the path matches what
    // was already rendered; explicit navigation to a different chat
    // destination (e.g. /chat/space/xyz) must still render normally.
    // The fragment is ignored: it is a one-off jump target (`#msg-…`), not
    // part of which page is showing.
    const oldPage = shell.querySelector('[data-scion-page]');
    if (
      returningFromTerminal &&
      oldPage &&
      oldPage.tagName.toLowerCase() === tag &&
      shell.currentPath.split('#')[0] === path.split('#')[0]
    ) {
      shell.user = currentUser;
      return;
    }

    shell.currentPath = path;
    shell.user = currentUser;

    // Replace the page content
    if (oldPage) oldPage.remove();

    const page = document.createElement(tag) as HTMLElement & { pageData: PageData };
    page.pageData = pageData;
    page.setAttribute('data-scion-page', '');
    shell.appendChild(page);
  } else {
    // Create the shell for the first time — clear any SSR-rendered content
    appContainer.innerHTML = '';
    const SHELL_TAGS: Record<ShellType, string> = {
      standalone: '', // handled above — standalone pages render without a shell
      chat: 'scion-chat-shell',
      profile: 'scion-profile-shell',
      app: 'scion-app',
    };
    const shellTag = SHELL_TAGS[shellType] ?? 'scion-app';
    const shell = document.createElement(shellTag) as HTMLElement & {
      currentPath: string;
      user: User | null;
    };
    shell.currentPath = path;
    shell.user = currentUser;

    const page = document.createElement(tag) as HTMLElement & { pageData: PageData };
    page.pageData = pageData;
    page.setAttribute('data-scion-page', '');
    shell.appendChild(page);
    appContainer.appendChild(shell);

    activeShell = { type: shellType, element: shell };
  }
}

/**
 * Sets up the client-side router for navigation
 */
function setupRouter(): void {
  // Add click handlers for client-side navigation.
  // Use the composed event path to find anchors inside shadow DOMs,
  // since target.closest('a') cannot cross shadow boundaries.
  document.addEventListener('click', (e: MouseEvent) => {
    const path = e.composedPath();
    let anchor: HTMLAnchorElement | null = null;
    for (const el of path) {
      if (el instanceof HTMLAnchorElement) {
        anchor = el;
        break;
      }
    }

    if (!anchor) return;
    if (
      e.defaultPrevented ||
      e.button !== 0 ||
      e.metaKey ||
      e.ctrlKey ||
      e.shiftKey ||
      e.altKey ||
      (anchor.target && anchor.target !== '_self') ||
      anchor.hasAttribute('download')
    )
      return;

    const href = anchor.getAttribute('href');
    if (!href) return;

    // Skip external links
    if (href.startsWith('http') || href.startsWith('//')) return;

    // Skip special links
    if (href.startsWith('javascript:')) return;
    if (href.startsWith('#')) return;

    // Skip links that should trigger full page loads
    if (href.startsWith('/api/')) return;
    if (href.startsWith('/auth/')) return;
    if (href.startsWith('/events')) return;

    // Handle client-side navigation
    e.preventDefault();
    navigateTo(href);
  });

  // Handle nav-click events from shadow DOM components (e.g. sidebar nav)
  // These events use composed:true to cross shadow boundaries.
  document.addEventListener('nav-click', ((e: CustomEvent<{ path: string }>) => {
    const path = e.detail?.path;
    if (path) {
      navigateTo(path);
    }
  }) as EventListener);

  // Handle browser back/forward.
  // Include query string so terminal layout state can be restored (#1715).
  // A page's own in-page entry on the path already shown (a chat panel on a
  // phone) is restored by that page; rendering would rebuild it.
  window.addEventListener('popstate', (e: PopStateEvent) => {
    const path = stripBasePath(window.location.pathname);
    const search = window.location.search;
    const appPath = search ? `${path}${search}` : path;
    if (
      isInPagePop(
        e.state,
        appPath,
        activeShell?.element as RouteShell | undefined,
        routeOutlet?.hidden ?? true
      )
    ) {
      // Back/Forward supersedes a render still in flight (a route the user
      // left this page for a moment ago), as any navigation does: it must
      // not replace the page this entry belongs to once it finishes.
      ++navigationId;
      return;
    }
    void renderRoute(appPath);
  });
}

/**
 * Navigates to a new path using the History API
 */
function navigateTo(path: string): void {
  const currentAppPath = stripBasePath(window.location.pathname);
  if (path === currentAppPath) {
    if (path === '/terminals' || terminalAgentRoute.test(path)) void renderRoute(path);
    return;
  }

  // Prefix app-relative paths with the base path for the browser URL bar
  window.history.pushState({}, '', browserPath(path));
  void renderRoute(path);
}

/**
 * Rewrites the current URL to an equivalent app path without rendering
 * anything, keeping its query and hash, for a page that already shows what
 * the new path names. Records the path as the active shell's rendered path,
 * as a render would, so returning to it (e.g. from the terminal workspace)
 * still reuses the page. Resolves once the shell has re-rendered for it.
 */
function replaceRoute(path: string): Promise<void> {
  const search = window.location.search;
  window.history.replaceState(
    window.history.state,
    '',
    browserPath(path) + search + window.location.hash
  );
  const shell = activeShell?.element as
    | (HTMLElement & { currentPath: string; updateComplete?: Promise<unknown> })
    | undefined;
  if (!shell) return Promise.resolve();
  shell.currentPath = search ? `${path}${search}` : path;
  return Promise.resolve(shell.updateComplete).then(() => undefined);
}

/**
 * Pushes a new history entry for an app path without rendering anything, for
 * a page that has already switched itself to what the path names (e.g. the
 * chat page opening another thread in place). Records the path as the active
 * shell's rendered path, as a render would, so the header's mode switch
 * remembers it and returning from the terminal workspace reuses the page.
 * `state` becomes the entry's history state. Resolves once the shell has
 * re-rendered for it.
 */
function pushRoute(path: string, state: Record<string, unknown> = {}): Promise<void> {
  return pushRouteEntry(
    activeShell?.element as RouteShell | undefined,
    path,
    browserPath(path),
    state
  );
}

// Initialize when DOM is ready
if (document.readyState === 'loading') {
  document.addEventListener('DOMContentLoaded', () => {
    void init();
  });
} else {
  void init();
}

// Re-export the central terminal helpers so tests and non-component callers
// can reach them through the entry module without importing a second path.
export { openTerminal, terminalHref } from './open-terminal.js';

// Export for use in components and tests
export { getInitialData, navigateTo, pushRoute, replaceRoute, stateManager };
