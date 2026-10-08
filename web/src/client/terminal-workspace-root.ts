import type { User } from '../shared/types.js';
import {
  TerminalSessionRegistry,
  AGENT_UNAVAILABLE_REASONS,
  AGENT_STOPPED_MESSAGE,
  type TerminalConnectionState,
  type TerminalSession,
  type TerminalSessionState,
  type TerminalDisconnectReason,
} from './terminal-sessions.js';
import {
  TerminalLayoutManager,
  type TerminalLayout,
  type TerminalSlot,
  serializeLayoutUrl,
} from './terminal-layout.js';
import type { TerminalAgentMetadata } from './terminal-metadata.js';
import type { ScionHeader } from '../components/shared/header.js';
import type { ScionTerminalPane } from '../components/terminal/terminal-pane.js';
import {
  TERMINAL_SESSION_COUNT_EVENT,
  TERMINAL_DRAG_MIME,
  TERMINAL_PALETTE_NEW_AGENT_EVENT,
  type TerminalSessionCountDetail,
  type TerminalPaletteNewAgentDetail,
} from './terminal-workspace-events.js';
import { enterAppFrame, exitAppFrame } from '../components/shared/app-frame.js';
import type { PaletteCandidate } from './chat-palette-types.js';
import {
  QuickPaletteHost,
  isQuickPaletteShortcut,
  type QuickPaletteLoadContext,
} from '../components/shared/palette/quick-palette-host.js';
import '../components/shared/header.js';
import { isMacPlatform } from '../utils/platform.js';
import { TOUCH_PRIMARY_QUERY } from '../utils/input-modality.js';
import { showConfirm } from '../components/shared/confirm-dialog.js';
import '../components/terminal/terminal-pane.js';

interface RailEntry {
  session: TerminalSession;
  state: TerminalSessionState;
  metadata: TerminalAgentMetadata;
  unsubscribeState: () => void;
  unsubscribeMetadata: () => void;
  /** Monotonic insertion index for stable chronological sorting. */
  addedAt: number;
}

/** The parts of a rail entry the bulk-action eligibility rules read. */
type RailEntryStatus = Pick<RailEntry, 'state' | 'metadata'>;

/**
 * Whether the per-row Reconnect action applies: the session has dropped
 * (disconnected or unavailable) and was not ended by an agent deletion.
 * Pending, connecting, connected, closed and idle entries are excluded;
 * idle entries connect through selection instead.
 */
export function canReconnectEntry(entry: RailEntryStatus): boolean {
  const { connection, disconnectReason } = entry.state;
  return (
    (connection === 'disconnected' || connection === 'unavailable') &&
    disconnectReason !== 'agent-deleted'
  );
}

/**
 * Whether "Reconnect all" acts on an entry: the agent still exists and the
 * entry is not connected. That is every entry the per-row Reconnect
 * applies to, plus idle entries restored from the saved list, which the
 * rail shows as "Not connected" (one at a time they connect through
 * selection, but "Reconnect all" is how a user connects them in bulk).
 */
export function isBulkReconnectEligible(entry: RailEntryStatus): boolean {
  if (entry.metadata.availability === 'deleted') return false;
  return canReconnectEntry(entry) || entry.state.connection === 'idle';
}

/**
 * Whether metadata says an entry's agent is gone: deleted, or in the stopped
 * or error phase. Availability "unavailable" does not count: it is also set
 * for transient failures (a failed metadata stream handshake, a 401, a 5xx
 * or a network error), so a brief hub outage after a reload must not make
 * restored rows for running agents removable.
 */
export function isAgentGone(metadata: TerminalAgentMetadata): boolean {
  if (metadata.availability === 'deleted') return true;
  const phase = metadata.agent?.phase;
  return phase === 'stopped' || phase === 'error';
}

/**
 * Whether "Remove all inactive" removes an entry: its agent was deleted, its
 * session dropped (disconnected or unavailable), or it is idle (restored
 * from the saved list and shown as "Not connected") and metadata says its
 * agent is gone. After a page load every row but the frontmost is idle, so
 * an idle row for an agent that is still running stays. Connected entries
 * stay, as do entries that are still connecting.
 */
export function isInactiveEntry(entry: RailEntryStatus): boolean {
  const { connection, disconnectReason } = entry.state;
  if (connection === 'closed') return false;
  if (connection === 'idle') return isAgentGone(entry.metadata);
  return (
    entry.metadata.availability === 'deleted' ||
    disconnectReason === 'agent-deleted' ||
    connection === 'disconnected' ||
    connection === 'unavailable'
  );
}

/** Tooltip and described-by text for a bulk button with nothing to act on. */
export const BULK_RECONNECT_DISABLED_REASON = 'No disconnected terminals to reconnect';
export const BULK_REMOVE_DISABLED_REASON = 'No inactive terminals to remove';

/** Gives each workspace root unique ids for its described-by targets. */
let nextInstanceId = 0;

/** Whether a bulk button is in its disabled (nothing to act on) state. */
function isBulkActionDisabled(button: HTMLButtonElement): boolean {
  return button.getAttribute('aria-disabled') === 'true';
}

// TERMINAL_DRAG_MIME imported from ./terminal-workspace-events.js

/** Preset button definitions for the layout toolbar. */
const PRESET_BUTTONS: ReadonlyArray<{ preset: TerminalLayout; label: string; title: string }> = [
  { preset: 'single', label: '1', title: 'Single pane' },
  { preset: 'two-columns', label: '2 side by side', title: 'Two columns' },
  { preset: 'two-rows', label: '2 stacked', title: 'Two rows' },
  { preset: 'four', label: '4', title: '2×2 grid' },
];

/**
 * Grid CSS templates for each preset.
 * Keys: [grid-template-columns, grid-template-rows]
 */
const GRID_TEMPLATES: Record<TerminalLayout, [string, string]> = {
  single: ['1fr', '1fr'],
  'two-columns': ['1fr 1fr', '1fr'],
  'two-rows': ['1fr', '1fr 1fr'],
  four: ['1fr 1fr', '1fr 1fr'],
};

/**
 * CSS grid placement for each slot index within a preset.
 * Each entry: [grid-column, grid-row]
 */
const SLOT_PLACEMENTS: Record<TerminalLayout, ReadonlyArray<[string, string]>> = {
  single: [['1', '1']],
  'two-columns': [
    ['1', '1'],
    ['2', '1'],
  ],
  'two-rows': [
    ['1', '1'],
    ['1', '2'],
  ],
  four: [
    ['1', '1'],
    ['2', '1'],
    ['1', '2'],
    ['2', '2'],
  ],
};

/** Document-lived presentation. Pane nodes never move through disposable shells. */
export class TerminalWorkspaceRoot {
  readonly element = document.createElement('div');
  private readonly header: ScionHeader = document.createElement('scion-header');
  private readonly shell = document.createElement('div');
  private readonly rail = document.createElement('aside');
  private readonly railList = document.createElement('div');
  private readonly railFooter = document.createElement('div');
  private readonly count = document.createElement('span');
  private readonly railBulk = document.createElement('div');
  private readonly bulkReconnect = document.createElement('button');
  private readonly bulkRemove = document.createElement('button');
  /** Disabled-reason tooltips around each bulk button (see buildRailBulkActions). */
  private readonly bulkReconnectTip = document.createElement('sl-tooltip');
  private readonly bulkRemoveTip = document.createElement('sl-tooltip');
  private readonly instanceId = nextInstanceId++;
  private readonly empty = document.createElement('div');
  private readonly layoutBar = document.createElement('div');
  private readonly paneHost = document.createElement('section');
  private readonly status = document.createElement('p');
  private readonly panes = new Map<string, ScionTerminalPane>();
  private readonly entries = new Map<string, RailEntry>();
  private readonly placeholders = new Map<number, HTMLElement>();
  private readonly ariaLive = document.createElement('div');
  private readonly placeMenu = document.createElement('div');
  /**
   * The "Jump to agent" palette. Its component and data modules load on
   * first open, so they stay out of the main bundle.
   */
  private readonly paletteHost = new QuickPaletteHost({
    mount: this.element,
    label: 'Jump to agent',
    placeholder: 'Search agents…',
    load: async (context): Promise<PaletteCandidate[]> => {
      this.paletteLoad = context;
      try {
        const { loadTerminalPaletteAgents } = await import('./terminal-palette-data.js');
        return await loadTerminalPaletteAgents(context);
      } finally {
        if (this.paletteLoad === context) this.paletteLoad = null;
      }
    },
    onSelect: (target): void => {
      this.paletteFocusAgentId = target.agentId;
      this.paletteDialogSettled = false;
      this.selectFromPalette(target.agentId);
    },
    onSelectionSettled: (): void => {
      this.paletteDialogSettled = true;
      this.focusPaletteTarget();
    },
  });
  /**
   * The most recent pane to receive real DOM focus, tracked continuously via
   * a persistent `focusin` listener (installed in the constructor) rather
   * than read reactively at open/select time — see that listener's own doc
   * comment for why a point-in-time read is unreliable here. Cleared when
   * that session closes ({@link syncSessions}).
   */
  private lastFocusedPaneSessionKey: string | null = null;
  /** The palette's Agents load in flight, if any; its result supersedes a live update. */
  private paletteLoad: QuickPaletteLoadContext | null = null;
  /**
   * Releases the agent store's hub entry, which the workspace retains while
   * it is shown, so the palette opens from memory and stays current. Set
   * as soon as the retain is requested: the store module loads on first
   * show, outside the main bundle.
   */
  private paletteAgentsRelease: (() => void) | null = null;
  /**
   * The agent picked from the palette, whose pane takes focus once it is
   * visible and the palette's close has settled — see
   * {@link focusPaletteTarget}.
   */
  private paletteFocusAgentId: string | null = null;
  /** Whether the palette's close animation and Shoelace's own focus restore have both finished. */
  private paletteDialogSettled = false;
  /**
   * The agent last selected in the open terminals rail (click, or Enter or
   * Space on its button), whose terminal takes keyboard focus once its
   * pane is visible — see {@link focusRailTarget}. Only a rail selection
   * sets it, so reconnects, restores and other ways of opening a pane never
   * move focus. Cleared once used, when focus lands somewhere other than
   * the rail or that pane, when the palette opens, when the workspace is
   * hidden, and when that agent's entry is removed (the rail close button
   * removes it at once), so a pane that turns up much later cannot take
   * focus.
   */
  private railFocusAgentId: string | null = null;
  /**
   * Multi-pane placement for a palette-picked agent with no session yet:
   * set by {@link selectFromPalette} before it asks `main.ts` to open the
   * agent, and consumed by {@link create} for that agent only — see that
   * method's own doc comment.
   */
  private palettePlacement: { agentId: string; focusedSessionKey: string | null } | null = null;
  readonly layoutManager = new TerminalLayoutManager();
  private registryUnsubscribe: (() => void) | null = null;
  private registry: TerminalSessionRegistry | null = null;
  private currentPath = '/terminals';
  private refreshQueued = false;
  private narrowQuery: MediaQueryList | null = null;
  /** {@link TOUCH_PRIMARY_QUERY}, kept live for the jump button's hints. */
  private touchQuery: MediaQueryList | null = null;
  private jumpButton: HTMLButtonElement | null = null;
  private jumpShortcutLabel = '';
  private jumpKeyShortcuts = '';
  private readonly handleTouchQueryChange = (): void => this.syncJumpButtonHints();
  /** Monotonic counter for stable chronological rail ordering. */
  private entryCounter = 0;
  /** Current rail sort mode. */
  private railSort: 'added' | 'alpha' | 'activity' = 'added';

  /**
   * Guard flag to prevent infinite loops when URL sync triggers a layout
   * update that would in turn trigger another URL sync. Set to true while
   * programmatically restoring layout from URL state.
   */
  private suppressUrlSync = false;

  /**
   * True while restore() is creating background (deferConnect) entries via
   * the coordinator. Suppresses syncSessions()'s auto-select-last-session
   * behavior so a restored entry
   * does not steal the frontmost slot; the persistence module selects the
   * saved frontmost explicitly, outside this suspension.
   */
  private autoSelectSuspended = false;

  private user: User | null = null;

  /**
   * Tracks whether this root currently holds a frame-mode reference, so
   * `show()` only calls `enterAppFrame()`/`exitAppFrame()` on an actual
   * visibility transition — repeated `show(true)` calls for successive
   * `/terminals` navigations (see `main.ts`'s router) must not inflate the
   * shared ref count.
   */
  private _frameEntered = false;

  constructor(user: User | null = null) {
    this.user = user;
    this.element.id = 'terminal-workspace';
    this.element.hidden = true;
    this.element.style.cssText =
      'height:var(--scion-app-height, 100dvh);min-height:0;display:none;flex-direction:column';
    this.element.className = 'terminal-workspace-root';
    // Expose workspace root on the element for coordinator and test access.
    (this.element as HTMLElement & { workspaceRoot?: TerminalWorkspaceRoot }).workspaceRoot = this;

    this.installStyles();
    this.header.user = user;
    this.header.currentPath = this.currentPath;
    this.header.pageTitle = '🌱 Scion Terminal Viewer';
    this.header.showMobileMenu = false;

    this.shell.className = 'terminal-workspace-shell';
    this.rail.className = 'terminal-rail';
    this.rail.setAttribute('aria-label', 'Open terminal sessions');
    const railHeader = document.createElement('div');
    railHeader.className = 'terminal-rail-header';
    const title = document.createElement('h2');
    title.textContent = 'Open terminals';
    this.count.className = 'terminal-count';
    const railSortWidget = this.buildRailSortWidget();
    railHeader.append(title, this.count, railSortWidget);
    this.railList.className = 'terminal-rail-list';
    this.railList.setAttribute('role', 'list');
    this.railList.setAttribute('aria-label', 'Retained terminal sessions');
    this.railList.addEventListener('keydown', (event) => this.handleRailKeydown(event));
    this.empty.className = 'terminal-empty';
    this.empty.textContent = 'No terminals are open.';
    this.buildRailFooter();
    this.buildRailBulkActions();
    this.rail.append(railHeader, this.railBulk, this.railList, this.railFooter);

    // Layout toolbar
    this.layoutBar.className = 'terminal-layout-bar';
    this.layoutBar.setAttribute('role', 'toolbar');
    this.layoutBar.setAttribute('aria-label', 'Layout presets');
    this.buildLayoutButtons();

    // Pane host: CSS Grid container
    this.paneHost.className = 'terminal-pane-host';
    this.status.className = 'terminal-status';
    this.status.textContent = 'No terminal selected.';
    this.paneHost.append(this.empty, this.status);
    // Aria-live region for placement announcements
    this.ariaLive.className = 'terminal-aria-live';
    this.ariaLive.setAttribute('aria-live', 'polite');
    this.ariaLive.setAttribute('role', 'status');

    // Place-in-pane menu (hidden by default)
    this.placeMenu.className = 'terminal-place-menu';
    this.placeMenu.setAttribute('role', 'menu');
    this.placeMenu.setAttribute('aria-label', 'Choose a slot');
    this.placeMenu.hidden = true;
    this.placeMenu.addEventListener('keydown', (e) => this.handlePlaceMenuKeydown(e));
    // Close menu on outside click
    document.addEventListener('click', (e) => {
      if (!this.placeMenu.hidden && !this.placeMenu.contains(e.target as Node)) {
        this.closePlaceMenu();
      }
    });

    // "Jump to agent" palette: agents-only, no DMs/Threads/People. Created
    // lazily on first open; opened by the rail footer button (see
    // buildRailFooter) or the keyboard shortcut (handleGlobalKeydown).
    document.addEventListener('keydown', this.handleGlobalKeydown);
    document.addEventListener('focusin', this.handleGlobalFocusIn);

    this.shell.append(this.rail, this.createPaneArea());
    this.element.append(this.header, this.shell, this.ariaLive, this.placeMenu);

    // Subscribe to layout state (lives as long as the workspace root).
    // Also sync URL on layout changes (#1715).
    this.layoutManager.subscribe(() => {
      this.queueRefresh();
      this.syncUrlFromLayout();
    });

    // Narrow-screen media query
    this.narrowQuery = window.matchMedia('(max-width: 760px)');
    this.narrowQuery.addEventListener('change', () => this.queueRefresh());

    this.refresh();
  }

  /** Creates the right-side area with the layout bar above the pane host. */
  private createPaneArea(): HTMLElement {
    const area = document.createElement('div');
    area.className = 'terminal-pane-area';
    area.append(this.layoutBar, this.paneHost);
    return area;
  }

  /** Builds the 4 preset buttons in the layout toolbar. */
  private buildLayoutButtons(): void {
    const label = document.createElement('span');
    label.className = 'terminal-layout-label';
    label.textContent = 'Layout:';
    this.layoutBar.append(label);

    for (const { preset, label: text, title } of PRESET_BUTTONS) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'terminal-layout-btn';
      btn.dataset.preset = preset;
      btn.textContent = text;
      btn.title = title;
      btn.setAttribute('aria-label', title);
      btn.addEventListener('click', () => {
        this.layoutManager.setLayout(preset);
      });
      this.layoutBar.append(btn);
    }

    // Restore button for zoom
    const restoreBtn = document.createElement('button');
    restoreBtn.type = 'button';
    restoreBtn.className = 'terminal-layout-restore';
    restoreBtn.textContent = 'Restore';
    restoreBtn.title = 'Exit zoom and restore layout';
    restoreBtn.setAttribute('aria-label', 'Exit zoom and restore layout');
    restoreBtn.hidden = true;
    restoreBtn.addEventListener('click', () => {
      this.layoutManager.unzoom();
    });
    this.layoutBar.append(restoreBtn);
  }

  /**
   * Builds the footer pinned below the rail list: a labelled "Jump to agent"
   * button that opens the agents palette. The rail is a flex column whose
   * list alone scrolls, so the footer stays visible however long the list
   * grows. The shortcut hint is shown inline on pointer devices and hidden
   * on touch-primary ones (CSS), where there is no keyboard to press it.
   * The title and aria-keyshortcuts follow the same rule, see
   * {@link syncJumpButtonHints}.
   */
  private buildRailFooter(): void {
    this.railFooter.className = 'terminal-rail-footer';
    const isMac = isMacPlatform();
    const shortcutLabel = isMac ? '⌘K' : 'Ctrl+K';
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'terminal-jump-btn';
    btn.setAttribute('aria-haspopup', 'dialog');
    this.jumpButton = btn;
    this.jumpShortcutLabel = shortcutLabel;
    this.jumpKeyShortcuts = isMac ? 'Meta+K' : 'Control+K';
    this.touchQuery = window.matchMedia?.(TOUCH_PRIMARY_QUERY) ?? null;
    this.touchQuery?.addEventListener?.('change', this.handleTouchQueryChange);
    this.syncJumpButtonHints();
    const icon = document.createElement('sl-icon');
    icon.setAttribute('name', 'compass');
    icon.setAttribute('aria-hidden', 'true');
    const label = document.createElement('span');
    label.className = 'terminal-jump-label';
    label.textContent = 'Jump to agent';
    const shortcut = document.createElement('kbd');
    shortcut.className = 'terminal-jump-shortcut';
    shortcut.setAttribute('aria-hidden', 'true');
    shortcut.textContent = shortcutLabel;
    btn.append(icon, label, shortcut);
    btn.addEventListener('click', () => this.handleJumpButtonClick(btn));
    this.railFooter.append(btn);
  }

  /**
   * Sets the jump button's title and aria-keyshortcuts only when the
   * primary pointer is not touch: a touch user cannot press the shortcut,
   * and the visible label already gives the accessible name. Re-run on
   * every change of {@link TOUCH_PRIMARY_QUERY}, like the CSS kbd hint.
   */
  private syncJumpButtonHints(): void {
    const btn = this.jumpButton;
    if (!btn) return;
    if (this.touchQuery?.matches) {
      btn.removeAttribute('title');
      btn.removeAttribute('aria-keyshortcuts');
    } else {
      btn.title = `Jump to agent (${this.jumpShortcutLabel})`;
      btn.setAttribute('aria-keyshortcuts', this.jumpKeyShortcuts);
    }
  }

  /**
   * iOS and macOS Safari do not focus a `<button>` on click, so the palette
   * would otherwise restore focus on close to whatever was focused before
   * (possibly a terminal pane). Focusing the button first makes it the
   * palette's invoker. {@link handleGlobalFocusIn} has already recorded
   * the last focused pane, so this does not lose the placement target.
   */
  private handleJumpButtonClick(btn: HTMLButtonElement): void {
    btn.focus({ preventScroll: true });
    this.openPalette();
  }

  /** Build the sort dropdown widget for the rail header. */
  private buildRailSortWidget(): HTMLElement {
    const wrapper = document.createElement('sl-dropdown');
    wrapper.className = 'terminal-sort-dropdown';

    const trigger = document.createElement('sl-icon-button');
    trigger.setAttribute('slot', 'trigger');
    trigger.setAttribute('name', 'sort-down');
    trigger.setAttribute('label', 'Sort rail');
    trigger.className = 'terminal-sort-btn';

    const menu = document.createElement('sl-menu');
    menu.className = 'terminal-sort-menu';

    const label = document.createElement('sl-menu-label');
    label.textContent = 'Sort by';
    menu.append(label);

    const options: Array<{ value: 'added' | 'alpha' | 'activity'; label: string }> = [
      { value: 'added', label: 'Added' },
      { value: 'alpha', label: 'Alphabetical' },
      { value: 'activity', label: 'Last activity' },
    ];

    for (const opt of options) {
      const item = document.createElement('sl-menu-item');
      item.setAttribute('type', 'checkbox');
      item.setAttribute('value', opt.value);
      item.textContent = opt.label;
      if (opt.value === this.railSort) item.setAttribute('checked', '');
      menu.append(item);
    }

    menu.addEventListener('sl-select', (e: Event) => {
      const detail = (e as CustomEvent<{ item?: HTMLElement }>).detail;
      const value = detail?.item?.getAttribute('value');
      if (value === 'added' || value === 'alpha' || value === 'activity') {
        this.railSort = value;
        // Update checked state on all menu items
        for (const mi of menu.querySelectorAll('sl-menu-item')) {
          if (mi.getAttribute('value') === value) {
            mi.setAttribute('checked', '');
          } else {
            mi.removeAttribute('checked');
          }
        }
        this.refresh();
      }
    });

    wrapper.append(trigger, menu);
    return wrapper;
  }

  setUser(user: User | null): void {
    this.user = user;
    this.header.user = user;
    for (const pane of this.panes.values()) {
      pane.userId = user?.id ?? '';
    }
  }

  setCurrentPath(path: string): void {
    this.currentPath = path;
    this.header.currentPath = path;
  }

  /**
   * Create a retained pane for `agentId`. Called by the coordinator adapter
   * (`main.ts`) for every brand-new session, regardless of what triggered it
   * — rail navigation, a URL/layout restore, or the "Jump to agent" palette.
   *
   * Placement differs for two cases, checked in order:
   *
   * 1. `options?.deferConnect` — a background restore entry (see
   *    `TerminalWorkspacePersistence`): it must not become visible or
   *    selected, so neither placement path below runs at all. The caller
   *    selects the frontmost entry separately.
   * 2. A pending palette placement for this same `agentId` — set by
   *    `selectFromPalette` (multi-pane only) before it asks `main.ts` to
   *    open the agent, consumed here to place the new pane via
   *    `addOrReplaceFocused` (fill next empty / replace focused) instead of
   *    `open()`'s overflow-to-single default. This hint travels through a
   *    field rather than a `create()` parameter because the coordinator's
   *    `TerminalCoordinatorAdapter.create` signature is shared by every
   *    entry point and crosses tabs via `BroadcastChannel` — placement is a
   *    purely local, same-tab UI decision with no meaning in any other tab.
   *    `create()` runs asynchronously after `coordinator.open()` is called
   *    (the coordinator awaits its lock claim and then defers `create()` to
   *    a microtask), so the hint is keyed by agent ID: a `create()` for any
   *    other agent in between never consumes it. `main.ts` clears it once
   *    that open settles. A palette selection never sets `deferConnect`, so
   *    the two never compete.
   *
   * Everything else falls through to `open()`'s existing overflow-to-single
   * default, unchanged.
   */
  create(
    registry: TerminalSessionRegistry,
    agentId: string,
    options?: { deferConnect?: boolean }
  ): TerminalSession {
    this.bindRegistry(registry);
    const pane = document.createElement('scion-terminal-pane');
    pane.className = 'terminal-pane';
    pane.style.cssText = 'height:100%;width:100%;min-height:0';
    pane.userId = this.user?.id ?? '';
    pane.setVisible(false);
    this.paneHost.appendChild(pane);
    try {
      const session = pane.open(registry, agentId, options);
      this.panes.set(session.state.key, pane);
      if (options?.deferConnect) {
        // See this method's own doc comment, case 1.
      } else if (this.palettePlacement?.agentId === agentId) {
        const { focusedSessionKey } = this.palettePlacement;
        this.palettePlacement = null;
        this.layoutManager.addOrReplaceFocused(session.state.key, focusedSessionKey);
      } else {
        // Check overflow: if the current multi preset is at capacity, switch to
        // single so the newly opened agent is visible.  Multi-pane assignments
        // are preserved — the user can switch back to see the prior grid.
        this.layoutManager.open(session.state.key);
      }
      return session;
    } catch (error) {
      pane.remove();
      throw error;
    }
  }

  /**
   * Runs fn with the auto-select-last-session behavior in syncSessions()
   * suspended, so entries created inside fn (typically background restore
   * entries) never displace whatever is already selected. See
   * autoSelectSuspended.
   */
  withAutoSelectSuspended<T>(fn: () => T): T {
    const previous = this.autoSelectSuspended;
    this.autoSelectSuspended = true;
    try {
      return fn();
    } finally {
      this.autoSelectSuspended = previous;
    }
  }

  select(session: TerminalSession): void {
    const pane = this.panes.get(session.state.key);
    if (!pane) throw new Error('Terminal session has no retained pane.');
    // Sets single[0] without changing the active preset (#1701).
    // Navigation of an already-open agent must not trigger overflow.
    this.layoutManager.select(session.state.key);
    this.status.textContent = '';
    this.show(true);
    this.refresh();
  }

  // ── "Jump to agent" palette ─────────────────────────────────────────────

  /**
   * Called by `main.ts` once the `coordinator.open()` for a palette-picked
   * agent settles, whether or not it ever reached `create()` (the agent may
   * be unreachable, unauthorized or deleted), so the hint can never leak
   * into a later, unrelated `create()` for that agent. A hint still in place
   * means no pane was created for the pick, so the pick's focus target is
   * dropped too: a pane that turns up later must not take focus away from
   * whatever the user has moved on to.
   */
  cancelPalettePlacement(agentId: string): void {
    if (this.palettePlacement?.agentId !== agentId) return;
    this.palettePlacement = null;
    if (this.paletteFocusAgentId === agentId) this.paletteFocusAgentId = null;
  }

  /**
   * Removes the document-level listeners this root installs. The root lives
   * for the whole tab in production; tests call this between instances.
   */
  dispose(): void {
    document.removeEventListener('keydown', this.handleGlobalKeydown);
    document.removeEventListener('focusin', this.handleGlobalFocusIn);
    this.releasePaletteAgents();
    this.touchQuery?.removeEventListener?.('change', this.handleTouchQueryChange);
    this.paletteHost.dispose();
  }

  /**
   * Retains the agent store's hub entry while the workspace is shown. The
   * store module is imported here, so it stays out of the main bundle; a
   * release before the import settles retains nothing.
   */
  private retainPaletteAgents(): void {
    if (this.paletteAgentsRelease) return;
    let release: (() => void) | null = null;
    let released = false;
    const handle = (): void => {
      released = true;
      release?.();
      release = null;
    };
    this.paletteAgentsRelease = handle;
    import('./terminal-palette-data.js')
      .then(({ retainTerminalPaletteAgents }) => {
        if (released) return;
        release = retainTerminalPaletteAgents((candidates) =>
          this.handlePaletteAgentsChange(candidates)
        );
      })
      .catch((err: unknown) => {
        if (this.paletteAgentsRelease === handle) this.paletteAgentsRelease = null;
        console.error('[Terminal] agent list unavailable for the palette:', err);
      });
  }

  private releasePaletteAgents(): void {
    const release = this.paletteAgentsRelease;
    this.paletteAgentsRelease = null;
    release?.();
  }

  /**
   * Keeps the open palette's Agents group current with the store, with no
   * request. Nothing is published while the palette is closed (the next
   * open reads the store) or while a load is in flight: its result
   * supersedes this one, and `setCandidates` would abort it. A load is
   * tracked from its start until it settles (a superseded load's settling
   * leaves the newer one tracked), so this needs no check of its own.
   */
  private handlePaletteAgentsChange(candidates: PaletteCandidate[]): void {
    if (!this.paletteHost.isOpen || this.paletteLoad) return;
    this.paletteHost.setCandidates(candidates);
  }

  /**
   * Tracks {@link lastFocusedPaneSessionKey} continuously as real DOM focus
   * moves, rather than reading it reactively at open or select time. Both
   * of those points are too late: opening the palette moves real focus into
   * its own query input (a correct focus trap, firing a real `focusout` on
   * whatever pane was focused), and *opening the palette via the rail
   * footer button* moves it there even earlier —
   * {@link handleJumpButtonClick} focuses the button itself, for its own
   * invoker-tracking purposes, before ever opening the palette. By either
   * point, a point-in-time "what pane has focus right now" read already
   * sees nothing. Recording it continuously instead,
   * every time focus actually lands in a pane, sidesteps both races — it
   * holds whatever pane was *last* focused regardless of what (if anything)
   * has stolen focus since.
   *
   * Once the palette's close has settled, focus landing in a pane other than
   * the picked agent's means the user has moved on, so the pick's pending
   * focus target is dropped. Before then, Shoelace's own focus restore may
   * land in the pane the palette was opened from, which is not a user move.
   */
  private readonly handleGlobalFocusIn = (e: FocusEvent): void => {
    // One pass over the path: note whether focus is in the rail list or in
    // a pane, and handle the pane it landed in.
    let inRailOrPane = false;
    for (const node of e.composedPath()) {
      if (node === this.railList) inRailOrPane = true;
      if (!(node instanceof Element) || node.tagName !== 'SCION-TERMINAL-PANE') continue;
      inRailOrPane = true;
      for (const [key, pane] of this.panes) {
        if (pane === node) {
          this.lastFocusedPaneSessionKey = key;
          if (
            this.railFocusAgentId !== null &&
            this.entries.get(key)?.state.agentId !== this.railFocusAgentId
          ) {
            this.railFocusAgentId = null;
          }
          if (
            this.paletteDialogSettled &&
            this.paletteFocusAgentId !== null &&
            this.entries.get(key)?.state.agentId !== this.paletteFocusAgentId
          ) {
            this.paletteFocusAgentId = null;
          }
          return;
        }
      }
    }
    // Focus moving anywhere outside the rail and the panes (another control,
    // a dialog) means the user has moved on: drop a pending rail target.
    if (!inRailOrPane) this.railFocusAgentId = null;
  };

  /**
   * Opens the palette (see {@link QuickPaletteHost.open}). The focused pane
   * itself (for a later `addOrReplaceFocused` call) is not captured here —
   * see {@link handleGlobalFocusIn}'s own doc comment for why it is instead
   * tracked continuously, as focus changes happen.
   */
  private openPalette(): void {
    if (this.paletteHost.isOpen) return;
    this.paletteFocusAgentId = null;
    this.railFocusAgentId = null;
    this.paletteHost.open();
  }

  /**
   * Focuses the palette-picked agent's pane once it is visible and the
   * palette's close has settled. Runs after the close settles and after
   * every refresh, since a new agent's pane may appear only later, once its
   * session is created.
   */
  private focusPaletteTarget(): void {
    const agentId = this.paletteFocusAgentId;
    if (!agentId || !this.paletteDialogSettled) return;
    const key = this.findSessionKeyByAgentId(agentId);
    const pane = key ? this.panes.get(key) : undefined;
    if (!pane || pane.hidden) return;
    this.paletteFocusAgentId = null;
    pane.focusTerminal();
  }

  /**
   * Moves keyboard focus into the terminal of the agent selected in the
   * rail once its pane is visible. Runs right after the rail selection (the
   * pane may already be on screen, and navigating to the route it already
   * shows changes nothing) and after every refresh, since a pane brought to
   * the front, or created for the selection, becomes visible only then.
   * `focusTerminal` focuses the xterm input, or the pane itself until the
   * terminal mounts, which then takes focus on its own.
   */
  private focusRailTarget(): void {
    const agentId = this.railFocusAgentId;
    if (!agentId) return;
    const key = this.findSessionKeyByAgentId(agentId);
    const pane = key ? this.panes.get(key) : undefined;
    if (!pane || pane.hidden || !this.layoutManager.getVisibleSlots().includes(key)) return;
    this.railFocusAgentId = null;
    pane.focusTerminal();
  }

  /**
   * Whether only one pane is on screen: the single preset, or a narrow
   * viewport, where {@link positionPanes} shows only the first occupied
   * slot of a multi-pane preset.
   */
  private isSinglePaneView(): boolean {
    return this.layoutManager.getState().active === 'single' || !!this.narrowQuery?.matches;
  }

  /**
   * Shows the picked agent.
   *
   * When only one pane is on screen, switches to the single preset and
   * navigates to `/terminals/<agentId>`, exactly like a rail click, so the
   * URL follows the shown agent and a pane placed in a multi-pane slot can
   * never end up off screen. Multi-pane assignments are kept for when the
   * user switches back.
   *
   * Otherwise ADDS the agent to the next empty pane; if the grid is already
   * full, REPLACES the focused pane instead of collapsing to single (see
   * `TerminalLayoutManager.addOrReplaceFocused`). An agent with an existing
   * session in this tab is placed directly — no new PTY connection is being
   * made, so there is nothing for the coordinator to arbitrate, the same
   * reasoning the rail's own drag-and-drop/"Place in pane" actions already
   * rely on. A brand new agent is routed through the coordinator exactly
   * like every other terminal-opening entry point, via
   * `TERMINAL_PALETTE_NEW_AGENT_EVENT` — `main.ts` is the only listener,
   * since it alone holds that module-local reference — and placed by
   * {@link create}.
   */
  private selectFromPalette(agentId: string): void {
    if (this.isSinglePaneView()) {
      this.layoutManager.setLayout('single');
      this.dispatchNavigation(`/terminals/${agentId}`);
      return;
    }
    const focusedSessionKey = this.lastFocusedPaneSessionKey;
    const existingKey = this.findSessionKeyByAgentId(agentId);
    if (existingKey) {
      this.layoutManager.addOrReplaceFocused(existingKey, focusedSessionKey);
      return;
    }
    this.palettePlacement = { agentId, focusedSessionKey };
    this.element.dispatchEvent(
      new CustomEvent<TerminalPaletteNewAgentDetail>(TERMINAL_PALETTE_NEW_AGENT_EVENT, {
        detail: { agentId },
        bubbles: true,
        composed: true,
      })
    );
  }

  // ── Keyboard shortcut: Cmd+K everywhere, Ctrl+K outside a pane ──────────
  // (and, on macOS, outside any editable text field)

  /**
   * Cmd+K (Meta+K) opens the palette everywhere, including with a terminal
   * pane focused: xterm never cancels or stops-propagating a plain Meta+K
   * (it has no C0/C1 mapping for it), so this plain bubble-phase listener
   * already sees it from inside a pane with no capture-phase trick needed.
   * Ctrl+K opens the palette only when focus is outside a pane and, on
   * macOS, outside any editable text field (see isQuickPaletteShortcut);
   * xterm's input textarea is one, so the two rules agree. xterm DOES
   * send Ctrl+K to the PTY (kill-line, `\x0b`) and then stops its own
   * propagation, so a pane-focused Ctrl+K never reaches here at all — the
   * explicit `eventFromTerminalPane` check below is belt-and-suspenders, not
   * what does the work. Only `ctrlKey` skips that check; `metaKey` must still
   * open the palette from inside a pane, so it is deliberately exempted.
   *
   * While the palette is open, the same shortcut closes it, as in chat. On
   * macOS, Ctrl+K in the palette's own search field edits the query, so
   * Cmd+K is what closes it from there.
   */
  private readonly handleGlobalKeydown = (e: KeyboardEvent): void => {
    if (this.element.hidden) return;
    if (!isQuickPaletteShortcut(e)) return;
    if (this.paletteHost.isOpen) {
      e.preventDefault();
      this.paletteHost.close();
      return;
    }
    if (e.ctrlKey && this.eventFromTerminalPane(e)) return;
    if (this.paletteHost.hasUnrelatedModalOpen()) return;
    e.preventDefault();
    this.openPalette();
  };

  /**
   * True when the event's real (composedPath) origin is inside a terminal
   * pane. Every xterm surface lives inside a `scion-terminal-pane`, so the
   * pane element alone identifies it.
   */
  private eventFromTerminalPane(e: KeyboardEvent): boolean {
    return e
      .composedPath()
      .some((node) => node instanceof Element && node.tagName === 'SCION-TERMINAL-PANE');
  }

  setStatus(message: string): void {
    const state = this.layoutManager.getState();
    const slots = this.layoutManager.getVisibleSlots();
    const hasSelected = slots.some((s) => s !== null);
    if (hasSelected) return;
    this.status.textContent = message;
    if (state.active === 'single' && state.single[0] === null) {
      this.refresh();
    }
  }

  show(visible: boolean): void {
    if (visible) {
      this.retainPaletteAgents();
    } else {
      this.paletteHost.hide();
      this.railFocusAgentId = null;
      this.releasePaletteAgents();
    }
    this.element.hidden = !visible;
    this.element.style.display = visible ? 'flex' : 'none';
    if (visible && !this._frameEntered) {
      this._frameEntered = true;
      enterAppFrame();
    } else if (!visible && this._frameEntered) {
      this._frameEntered = false;
      exitAppFrame();
    }
    this.refreshPaneVisibility();
  }

  private bindRegistry(registry: TerminalSessionRegistry): void {
    if (this.registry === registry) return;
    this.registryUnsubscribe?.();
    this.registry = registry;
    this.registryUnsubscribe = registry.subscribe((sessions) => this.syncSessions(sessions));
  }

  private syncSessions(sessions: readonly TerminalSession[]): void {
    const retained = new Set(sessions.map((session) => session.state.key));
    for (const [key, entry] of this.entries) {
      if (retained.has(key)) continue;
      entry.unsubscribeState();
      entry.unsubscribeMetadata();
      this.entries.delete(key);
      const pane = this.panes.get(key);
      pane?.remove();
      this.panes.delete(key);
      // Close in layout manager to clear all preset references
      this.layoutManager.close(key);
      if (this.lastFocusedPaneSessionKey === key) this.lastFocusedPaneSessionKey = null;
      // Closed from the rail, or removed elsewhere: a pane opened later for
      // the same agent must not take focus from this old selection.
      if (this.railFocusAgentId === entry.state.agentId) this.railFocusAgentId = null;
    }
    for (const session of sessions) {
      if (this.entries.has(session.state.key)) continue;
      const state = session.state;
      const metadata = this.registry!.metadata.get(state.agentId) ?? {
        agent: state.agent,
        availability: 'loading' as const,
        error: null,
      };
      const entry: RailEntry = {
        session,
        state,
        metadata,
        unsubscribeState: () => {},
        unsubscribeMetadata: () => {},
        addedAt: this.entryCounter++,
      };
      entry.unsubscribeState = session.subscribe((next) => {
        entry.state = next;
        this.queueRefresh();
      });
      entry.unsubscribeMetadata = this.registry!.metadata.subscribe(state.agentId, (next) => {
        entry.metadata = next;

        // SSE → session bridge: reconcile metadata availability with session connection
        try {
          if (
            next.availability === 'deleted' &&
            entry.session.state.connection !== 'closed' &&
            !(
              entry.session.state.connection === 'unavailable' &&
              entry.session.state.disconnectReason === 'agent-deleted'
            )
          ) {
            entry.session.markUnavailable('agent-deleted', next.error ?? 'Agent was deleted.');
          } else if (
            // A crashed container reports phase 'error', not 'stopped'
            // (ptone/scion#2096); treat both the same so a crashed agent's
            // pane also re-arms once it is running again.
            (next.agent?.phase === 'stopped' || next.agent?.phase === 'error') &&
            entry.session.state.connection !== 'closed' &&
            // Idle entries (restored, not yet connected) stay idle while
            // their agent is stopped: marking a never-connected entry
            // unavailable would strand it, since noteAgentAvailable()'s
            // re-arm requires everConnected. Selecting it later behaves like
            // opening a stopped agent's terminal today.
            entry.session.state.connection !== 'idle'
          ) {
            if (entry.session.state.connection !== 'unavailable') {
              entry.session.markUnavailable('agent-stopped', AGENT_STOPPED_MESSAGE);
            } else {
              // The session is already unavailable — most often because a
              // WebSocket close already reported agent_stopped before this
              // SSE update arrived. markUnavailable() would be a no-op here,
              // but this SSE update is still the independent down
              // observation noteAgentAvailable() requires before it will act
              // on a later "running" signal (ptone/scion#2096).
              entry.session.noteAgentDown();
            }
          } else if (
            // An agent that stops and restarts re-arms auto-reconnect once
            // it is confirmed running again. The WebSocket drop usually
            // reaches the client before this SSE update does, so the
            // session's own attempt often already classified itself as
            // agent-phase/agent-offline rather than markUnavailable's
            // agent-stopped — accept any agent-state unavailability reason.
            // Gate on activity too, so an offline-but-running agent does not
            // burn the single attempt.
            next.agent?.phase === 'running' &&
            next.agent?.activity !== 'offline' &&
            entry.session.state.connection === 'unavailable' &&
            AGENT_UNAVAILABLE_REASONS.has(entry.session.state.disconnectReason)
          ) {
            entry.session.noteAgentAvailable();
          }
        } catch {
          // markUnavailable should not throw, but guard the subscription callback
        }

        this.queueRefresh();
      });
      this.entries.set(session.state.key, entry);
    }
    // If no active session, auto-select via layout manager. Suspended while
    // restore() creates background entries (autoSelectSuspended), so a
    // restored entry never displaces the frontmost slot on its own.
    const currentSlots = this.layoutManager.getVisibleSlots();
    if (!currentSlots.some((s) => s !== null) && sessions.length > 0 && !this.autoSelectSuspended) {
      this.layoutManager.open(sessions[sessions.length - 1].state.key);
    }
    this.refresh();
  }

  private queueRefresh(): void {
    if (this.refreshQueued) return;
    this.refreshQueued = true;
    queueMicrotask(() => {
      this.refreshQueued = false;
      this.refresh();
    });
  }

  /** Sort rail entries in place according to the current railSort mode. */
  private sortEntries(entries: RailEntry[]): void {
    entries.sort((a, b) => {
      let cmp = 0;
      switch (this.railSort) {
        case 'added':
          cmp = a.addedAt - b.addedAt;
          break;
        case 'alpha': {
          const nameA = (a.metadata.agent?.name ?? a.state.agentId).toLowerCase();
          const nameB = (b.metadata.agent?.name ?? b.state.agentId).toLowerCase();
          cmp = nameA.localeCompare(nameB);
          break;
        }
        case 'activity': {
          const tsA = a.metadata.agent?.lastActivityEvent ?? a.metadata.agent?.lastSeen ?? '';
          const tsB = b.metadata.agent?.lastActivityEvent ?? b.metadata.agent?.lastSeen ?? '';
          // Numeric comparison handles mixed-precision ISO timestamps correctly
          // (e.g. "…T01:00:00Z" vs "…T01:00:00.500Z" where localeCompare fails
          // because '.' < 'Z' lexicographically). Missing/invalid → 0 (oldest).
          const dateA = tsA ? Date.parse(tsA) : 0;
          const dateB = tsB ? Date.parse(tsB) : 0;
          cmp = (isNaN(dateB) ? 0 : dateB) - (isNaN(dateA) ? 0 : dateA);
          break;
        }
      }
      // Deterministic tie-breaking: by agent name, then by session key
      if (cmp === 0) {
        const nameA = (a.metadata.agent?.name ?? a.state.agentId).toLowerCase();
        const nameB = (b.metadata.agent?.name ?? b.state.agentId).toLowerCase();
        cmp = nameA.localeCompare(nameB);
      }
      if (cmp === 0) {
        cmp = a.state.key.localeCompare(b.state.key);
      }
      return cmp;
    });
  }

  private refresh(): void {
    const entries = [...this.entries.values()];
    this.sortEntries(entries);
    const total = entries.length;
    const focusedId =
      document.activeElement instanceof HTMLElement &&
      this.railList.contains(document.activeElement)
        ? document.activeElement.dataset.railFocusId
        : null;
    this.count.textContent = String(total);
    this.updateRailBulkActions(entries, total);

    const layoutState = this.layoutManager.getState();
    const visibleSlots = this.layoutManager.getVisibleSlots();
    const hasSelected = visibleSlots.some((s) => s !== null);

    // Multi-pane layouts show a dotted placeholder in every empty slot, so
    // neither full-size overlay is shown there: both would cover the
    // placeholders and hide the drop targets, whether or not any terminal
    // is open yet. Narrow and zoomed views render a single slot with no
    // placeholder and keep the status message.
    const isMultiPane = layoutState.active !== 'single';
    const showsPlaceholders =
      isMultiPane &&
      !(this.narrowQuery?.matches ?? false) &&
      this.layoutManager.getZoomed() === null;
    this.empty.hidden = total > 0 || isMultiPane;
    this.status.hidden =
      (total > 0 && hasSelected) || (total === 0 && isMultiPane) || showsPlaceholders;

    // Rail rendering
    this.railList.replaceChildren(...entries.map((entry) => this.renderRailEntry(entry)));
    if (focusedId) {
      this.restoreRailFocus(focusedId);
      requestAnimationFrame(() => this.restoreRailFocus(focusedId));
    }

    // Update layout bar active state
    this.updateLayoutBar(layoutState);

    // Update grid template based on active preset (or single when zoomed)
    this.updateGridTemplate(layoutState);

    // Position panes and manage placeholders
    this.positionPanes(layoutState);

    // Visibility
    this.refreshPaneVisibility();
    this.publishCount();
    this.focusPaletteTarget();
    this.focusRailTarget();
  }

  /** Update layout toolbar button highlighting. */
  private updateLayoutBar(state: { active: TerminalLayout }): void {
    const isZoomed = this.layoutManager.getZoomed() !== null;

    for (const btn of this.layoutBar.querySelectorAll<HTMLButtonElement>('.terminal-layout-btn')) {
      const isActive = btn.dataset.preset === state.active && !isZoomed;
      btn.dataset.active = String(isActive);
      btn.setAttribute('aria-pressed', String(isActive));
    }

    const restoreBtn = this.layoutBar.querySelector<HTMLButtonElement>('.terminal-layout-restore');
    if (restoreBtn) restoreBtn.hidden = !isZoomed;
  }

  /** Set grid-template-columns/rows on the pane host based on preset. */
  private updateGridTemplate(state: { active: TerminalLayout }): void {
    const isNarrow = this.narrowQuery?.matches ?? false;
    const isZoomed = this.layoutManager.getZoomed() !== null;

    // In narrow or zoomed mode, show single-pane grid
    const effectivePreset: TerminalLayout = isNarrow || isZoomed ? 'single' : state.active;
    const [cols, rows] = GRID_TEMPLATES[effectivePreset];
    this.paneHost.style.gridTemplateColumns = cols;
    this.paneHost.style.gridTemplateRows = rows;

    // Expose effective layout so CSS can scope focus-outline to multi-pane modes.
    this.paneHost.dataset.effectiveLayout = effectivePreset;
  }

  /** Position each pane in the grid and manage empty slot placeholders. */
  private positionPanes(state: { active: TerminalLayout }): void {
    const isNarrow = this.narrowQuery?.matches ?? false;
    const isZoomed = this.layoutManager.getZoomed() !== null;
    const zoomedKey = this.layoutManager.getZoomed();
    const visibleSlots = this.layoutManager.getVisibleSlots();

    const effectivePreset: TerminalLayout = isNarrow || isZoomed ? 'single' : state.active;
    const placements = SLOT_PLACEMENTS[effectivePreset];

    // Determine the effective visible slots for rendering
    let renderSlots: readonly TerminalSlot[];
    if (isZoomed && zoomedKey) {
      renderSlots = [zoomedKey];
    } else if (isNarrow) {
      // In narrow mode, show first occupied pane from active preset
      const activeSlots = this.getActivePresetSlots(state.active);
      const firstOccupied = activeSlots.find((s) => s !== null);
      renderSlots = [firstOccupied ?? null];
    } else {
      renderSlots = visibleSlots;
    }

    // Track which slots need placeholders vs panes
    const visibleKeys = new Set<string>();
    const usedPlaceholderIndices = new Set<number>();

    for (let i = 0; i < renderSlots.length && i < placements.length; i++) {
      const key = renderSlots[i];
      const [col, row] = placements[i];

      if (key && this.panes.has(key)) {
        // Position the pane in the grid
        const pane = this.panes.get(key)!;
        pane.style.gridColumn = col;
        pane.style.gridRow = row;
        pane.style.display = '';
        pane.dataset.slotIndex = String(i);
        visibleKeys.add(key);

        // Install drop handlers on the pane element
        this.installDropHandlers(pane, i, effectivePreset);

        // Remove placeholder for this slot if exists
        const ph = this.placeholders.get(i);
        if (ph) {
          ph.remove();
          this.placeholders.delete(i);
        }
      } else if (effectivePreset !== 'single') {
        // Show placeholder for empty slot in multi-pane layouts.
        // In single layout with no session the "No terminals" empty state
        // covers the view, so a placeholder is unnecessary.
        usedPlaceholderIndices.add(i);
        let ph = this.placeholders.get(i);
        if (!ph) {
          ph = document.createElement('div');
          ph.className = 'terminal-slot-placeholder';
          this.paneHost.appendChild(ph);
          this.placeholders.set(i, ph);
        }
        ph.textContent = 'Drop terminal here';
        ph.style.gridColumn = col;
        ph.style.gridRow = row;
        ph.dataset.slotIndex = String(i);
        ph.hidden = false;

        // Install drop handlers on the placeholder
        this.installDropHandlers(ph, i, effectivePreset);
      }
    }

    // Hide placeholders not used by current layout
    for (const [idx, ph] of this.placeholders) {
      if (!usedPlaceholderIndices.has(idx)) {
        ph.remove();
        this.placeholders.delete(idx);
      }
    }

    // Hide panes not visible in the current render and clear stale slot attributes
    for (const [key, pane] of this.panes) {
      if (!visibleKeys.has(key)) {
        pane.style.display = 'none';
        delete pane.dataset.slotIndex;
        // Clean up drop handlers on hidden panes
        const cleanup = (pane as HTMLElement & { __dropCleanup?: () => void }).__dropCleanup;
        if (cleanup) {
          cleanup();
          delete (pane as HTMLElement & { __dropCleanup?: () => void }).__dropCleanup;
        }
      }
    }
  }

  /** Get the slot array for the active preset from layout state. */
  private getActivePresetSlots(preset: TerminalLayout): readonly TerminalSlot[] {
    const state = this.layoutManager.getState();
    switch (preset) {
      case 'single':
        return state.single;
      case 'two-columns':
        return state.twoColumns;
      case 'two-rows':
        return state.twoRows;
      case 'four':
        return state.four;
    }
  }

  /** Apply visibility to all panes based on layout state and workspace visibility. */
  private refreshPaneVisibility(): void {
    const workspaceVisible = !this.element.hidden;
    const isNarrow = this.narrowQuery?.matches ?? false;
    const isZoomed = this.layoutManager.getZoomed() !== null;
    const zoomedKey = this.layoutManager.getZoomed();
    const layoutState = this.layoutManager.getState();

    // Determine the set of keys that should be visible
    const visibleKeys = new Set<string>();

    if (workspaceVisible) {
      if (isZoomed && zoomedKey) {
        visibleKeys.add(zoomedKey);
      } else if (isNarrow) {
        // Show first occupied pane from active preset
        const activeSlots = this.getActivePresetSlots(layoutState.active);
        const firstOccupied = activeSlots.find((s) => s !== null);
        if (firstOccupied) visibleKeys.add(firstOccupied);
      } else {
        // All non-null slots in the active preset
        const slots = this.layoutManager.getVisibleSlots();
        for (const s of slots) {
          if (s !== null) visibleKeys.add(s);
        }
      }
    }

    for (const [key, pane] of this.panes) {
      pane.setVisible(visibleKeys.has(key));
    }
  }

  private restoreRailFocus(focusedId: string): void {
    const active = document.activeElement;
    if (
      active instanceof HTMLElement &&
      active !== document.body &&
      !this.railList.contains(active)
    )
      return;
    if (
      active instanceof HTMLElement &&
      this.railList.contains(active) &&
      active.dataset.railFocusId &&
      active.dataset.railFocusId !== focusedId
    )
      return;
    [...this.railList.querySelectorAll<HTMLElement>('[data-rail-focus-id]')]
      .find((element) => element.dataset.railFocusId === focusedId)
      ?.focus();
  }

  /** The rail's Reconnect action, shared by the row button and "Reconnect all". */
  private reconnectEntry(entry: RailEntry): void {
    void this.registry?.metadata.refresh(entry.state.agentId);
    void entry.session.connect();
  }

  /** The rail's Close action, shared by the row button and "Remove all inactive". */
  private removeEntry(entry: RailEntry): void {
    entry.session.close();
  }

  /**
   * Builds the bulk-action bar between the rail header and the list. Each
   * button sits above the row column it acts on (Reconnect, then Close).
   *
   * A button with nothing to act on is marked aria-disabled rather than
   * disabled, so it stays focusable and keyboard users can reach the
   * reason; its click handler checks the same flag. The reason shows in a
   * tooltip on a wrapper span (disabled controls do not get pointer events
   * in every browser) and is linked to the button with aria-describedby.
   */
  private buildRailBulkActions(): void {
    this.railBulk.className = 'terminal-rail-bulk';
    this.railBulk.setAttribute('role', 'toolbar');
    this.railBulk.setAttribute('aria-label', 'Bulk terminal actions');
    const label = document.createElement('span');
    label.className = 'terminal-rail-bulk-label';
    label.textContent = 'All terminals';
    label.setAttribute('aria-hidden', 'true');

    this.bulkReconnect.type = 'button';
    this.bulkReconnect.className = 'terminal-bulk-action terminal-bulk-reconnect';
    this.bulkReconnect.innerHTML = '<sl-icon name="arrow-repeat"></sl-icon>';
    this.bulkReconnect.addEventListener('click', () => {
      if (!isBulkActionDisabled(this.bulkReconnect)) this.reconnectAll();
    });

    this.bulkRemove.type = 'button';
    this.bulkRemove.className = 'terminal-bulk-action terminal-bulk-remove';
    this.bulkRemove.innerHTML = '<sl-icon name="trash"></sl-icon>';
    this.bulkRemove.addEventListener('click', () => {
      if (!isBulkActionDisabled(this.bulkRemove)) void this.removeAllInactive();
    });

    this.railBulk.append(
      label,
      this.wrapBulkAction(
        this.bulkReconnect,
        this.bulkReconnectTip,
        'terminal-bulk-reconnect-reason'
      ),
      this.wrapBulkAction(this.bulkRemove, this.bulkRemoveTip, 'terminal-bulk-remove-reason')
    );
  }

  /**
   * Wraps a bulk button as sl-tooltip > span > button, plus a visually
   * hidden reason element the button's aria-describedby points at.
   */
  private wrapBulkAction(
    button: HTMLButtonElement,
    tip: HTMLElement,
    reasonId: string
  ): HTMLElement {
    tip.className = 'terminal-bulk-tooltip';
    tip.setAttribute('placement', 'bottom');
    tip.setAttribute('hoist', '');
    tip.setAttribute('disabled', '');
    const wrap = document.createElement('span');
    wrap.className = 'terminal-bulk-action-wrap';
    const reason = document.createElement('span');
    reason.className = 'terminal-bulk-reason terminal-aria-live';
    reason.id = `${reasonId}-${this.instanceId}`;
    reason.hidden = true;
    button.setAttribute('aria-describedby', reason.id);
    wrap.append(button, reason);
    tip.append(wrap);
    return tip;
  }

  /**
   * Sets a bulk button's enabled state. Disabled: aria-disabled, the
   * reason in the tooltip and the described-by text, no native title (it
   * would show a second tooltip). Enabled: the tooltip is off and the
   * native title carries the usual hover help.
   */
  private setBulkActionState(
    button: HTMLButtonElement,
    tip: HTMLElement,
    disabledReason: string | null,
    enabledHelp: string
  ): void {
    const reason = button.parentElement?.querySelector<HTMLElement>('.terminal-bulk-reason');
    if (disabledReason) {
      button.setAttribute('aria-disabled', 'true');
      button.removeAttribute('title');
      tip.setAttribute('content', disabledReason);
      tip.removeAttribute('disabled');
      if (reason) {
        reason.textContent = disabledReason;
        reason.hidden = false;
      }
    } else {
      button.removeAttribute('aria-disabled');
      button.title = enabledHelp;
      tip.removeAttribute('content');
      tip.setAttribute('disabled', '');
      if (reason) {
        reason.textContent = '';
        reason.hidden = true;
      }
    }
  }

  /** Syncs the bulk buttons' enabled state and hover help with the entries. */
  private updateRailBulkActions(entries: readonly RailEntry[], total: number): void {
    this.railBulk.hidden = total === 0;
    const reconnectable = entries.filter(isBulkReconnectEligible).length;
    const inactive = entries.filter(isInactiveEntry).length;

    this.setBulkActionState(
      this.bulkReconnect,
      this.bulkReconnectTip,
      reconnectable === 0 ? BULK_RECONNECT_DISABLED_REASON : null,
      `Reconnect all: reconnect ${countLabel(reconnectable)} that ${
        reconnectable === 1 ? 'is' : 'are'
      } not connected and whose agent still exists. Connected terminals and terminals for deleted agents are left alone.`
    );
    this.bulkReconnect.setAttribute('aria-label', `Reconnect all (${reconnectable} eligible)`);

    this.setBulkActionState(
      this.bulkRemove,
      this.bulkRemoveTip,
      inactive === 0 ? BULK_REMOVE_DISABLED_REASON : null,
      `Remove all inactive: remove ${countLabel(inactive)} whose connection dropped or whose agent is stopped, errored or deleted. Connected terminals and not yet opened terminals for running agents stay.`
    );
    this.bulkRemove.setAttribute('aria-label', `Remove all inactive (${inactive} eligible)`);
  }

  /** "Reconnect all": runs the row Reconnect action on every eligible entry. */
  reconnectAll(): void {
    for (const entry of [...this.entries.values()].filter(isBulkReconnectEligible)) {
      this.reconnectEntry(entry);
    }
  }

  /**
   * "Remove all inactive": asks for confirmation, then runs the row Close
   * action on the entries that were inactive when the dialog opened.
   * After the dialog, only those entries that are still inactive are
   * removed: one that reconnected is kept, and one that dropped is not
   * removed because the dialog did not count it.
   * Resolves to the number of entries removed.
   */
  async removeAllInactive(): Promise<number> {
    const confirmedKeys = new Set(
      [...this.entries.values()].filter(isInactiveEntry).map((entry) => entry.state.key)
    );
    const count = confirmedKeys.size;
    if (count === 0) return 0;
    const confirmed = await showConfirm(
      `Remove ${countLabel(count)} from the list? This removes terminals whose connection dropped and terminals whose agent is stopped, errored or deleted. Connected terminals and not yet opened terminals for running agents stay.`,
      { title: 'Remove inactive terminals', confirmText: `Remove ${count}` }
    );
    if (!confirmed) return 0;
    const targets = [...this.entries.values()].filter(
      (entry) => confirmedKeys.has(entry.state.key) && isInactiveEntry(entry)
    );
    for (const entry of targets) this.removeEntry(entry);
    if (targets.length > 0) {
      // Render now so the focus target reflects the remaining rows.
      this.refresh();
      this.focusAfterBulkRemove();
    }
    return targets.length;
  }

  /**
   * The dialog returns focus to "Remove all inactive", which now has
   * nothing to act on. Move it to "Reconnect all"
   * when that is still enabled, else the first remaining row, else the
   * rail itself. Focus the user moved elsewhere is left alone.
   */
  private focusAfterBulkRemove(): void {
    const active = document.activeElement;
    if (active && active !== document.body && active !== this.bulkRemove) return;
    if (!this.railBulk.hidden && !isBulkActionDisabled(this.bulkReconnect)) {
      this.bulkReconnect.focus();
      return;
    }
    const firstRow = this.railList.querySelector<HTMLElement>('.terminal-rail-select');
    if (firstRow) {
      firstRow.focus();
      return;
    }
    this.rail.tabIndex = -1;
    this.rail.focus();
  }

  private renderRailEntry(entry: RailEntry): HTMLElement {
    const metadata = entry.metadata;
    const agent = metadata.agent ?? entry.state.agent;
    const agentName = agent?.name || entry.state.agentId;
    // Prefer the hub-resolved project name; fall back to the project id
    // when the name is not known yet, and omit the line when neither is.
    const projectLabel = agent?.project || agent?.projectId || '';
    const item = document.createElement('div');
    item.className = 'terminal-rail-item';
    item.setAttribute('role', 'listitem');
    // Mark as selected if this key is in the visible slots
    const visibleSlots = this.layoutManager.getVisibleSlots();
    item.dataset.selected = String(visibleSlots.includes(entry.state.key));
    item.dataset.connection = entry.state.connection;
    item.dataset.availability = metadata.availability;
    if (entry.state.disconnectReason) item.dataset.disconnectReason = entry.state.disconnectReason;

    // One list of status parts feeds both forms: the visible titles join them
    // with a middle dot, the accessible label with a comma so screen readers
    // pause between them instead of announcing or skipping the dot.
    const statusParts = [
      disconnectLabel(entry.state.connection, entry.state.disconnectReason),
      availabilityLabel(metadata.availability),
    ];
    const statusLabel = statusParts.join(' · ');
    const spokenStatus = statusParts.join(', ');

    const select = document.createElement('button');
    select.type = 'button';
    select.className = 'terminal-rail-select';
    // The status dot is small and aria-hidden, so surface its status text on
    // the whole row: as the hover title and in the accessible label.
    select.title = statusLabel;
    select.setAttribute(
      'aria-label',
      projectLabel
        ? `Show terminal for ${agentName} in ${projectLabel}, ${spokenStatus}`
        : `Show terminal for ${agentName}, ${spokenStatus}`
    );
    if (visibleSlots.includes(entry.state.key)) select.setAttribute('aria-current', 'page');
    select.dataset.railFocusId = `${entry.state.key}:select`;
    select.addEventListener('click', () => this.openSessionRoute(entry));

    const connection = document.createElement('span');
    connection.className = 'terminal-connection-dot';
    connection.title = statusLabel;
    connection.setAttribute('aria-hidden', 'true');
    const text = document.createElement('span');
    text.className = 'terminal-rail-text';
    const name = document.createElement('span');
    name.className = 'terminal-agent-name';
    name.textContent = agentName;
    text.append(name);
    if (projectLabel) {
      const project = document.createElement('span');
      project.className = 'terminal-project-name';
      project.textContent = projectLabel;
      project.title = projectLabel;
      text.append(project);
    }
    select.append(connection, text);

    const actions = document.createElement('span');
    actions.className = 'terminal-rail-actions';
    const reconnect = document.createElement('button');
    reconnect.type = 'button';
    reconnect.className = 'terminal-icon-action';
    reconnect.setAttribute('aria-label', `Reconnect ${agentName}`);
    reconnect.dataset.railFocusId = `${entry.state.key}:reconnect`;
    reconnect.title = 'Reconnect';
    reconnect.innerHTML = '<sl-icon name="arrow-clockwise"></sl-icon>';
    reconnect.disabled = !canReconnectEntry(entry);
    reconnect.addEventListener('click', (event) => {
      event.stopPropagation();
      this.reconnectEntry(entry);
    });
    const close = document.createElement('button');
    close.type = 'button';
    close.className = 'terminal-icon-action';
    close.setAttribute('aria-label', `Close ${agentName}`);
    close.dataset.railFocusId = `${entry.state.key}:close`;
    close.title = 'Close';
    close.innerHTML = '<sl-icon name="x-circle"></sl-icon>';
    close.addEventListener('click', (event) => {
      event.stopPropagation();
      this.removeEntry(entry);
    });
    // Drag handle
    const dragHandle = document.createElement('span');
    dragHandle.className = 'terminal-drag-handle';
    dragHandle.setAttribute('draggable', 'true');
    dragHandle.setAttribute('aria-label', `Drag to place ${agentName} in layout`);
    dragHandle.setAttribute('role', 'img');
    dragHandle.textContent = '⠿';
    dragHandle.title = 'Drag to place in layout';
    dragHandle.addEventListener('dragstart', (e) => {
      e.dataTransfer!.setData(TERMINAL_DRAG_MIME, entry.state.key);
      e.dataTransfer!.effectAllowed = 'move';
      item.dataset.dragging = 'true';
    });
    dragHandle.addEventListener('dragend', () => {
      delete item.dataset.dragging;
    });

    // Place in pane button (keyboard/touch accessible alternative)
    const placeBtn = document.createElement('button');
    placeBtn.type = 'button';
    placeBtn.className = 'terminal-icon-action terminal-place-btn';
    placeBtn.setAttribute('aria-label', `Place ${agentName} in pane`);
    placeBtn.dataset.railFocusId = `${entry.state.key}:place`;
    placeBtn.title = 'Place in pane…';
    placeBtn.innerHTML = '<sl-icon name="grid"></sl-icon>';
    placeBtn.addEventListener('click', (event) => {
      event.stopPropagation();
      this.openPlaceMenu(entry.state.key, agentName, placeBtn);
    });

    actions.append(placeBtn, reconnect, close);
    item.append(dragHandle, select, actions);
    return item;
  }

  /**
   * Rail selection (a click, or Enter or Space on the rail button, which
   * the browser turns into a click): navigates to the agent and moves
   * keyboard focus into its terminal — see {@link focusRailTarget}.
   */
  private openSessionRoute(entry: RailEntry): void {
    this.railFocusAgentId = entry.state.agentId;
    this.dispatchNavigation(`/terminals/${entry.state.agentId}`);
    this.focusRailTarget();
  }

  private dispatchNavigation(path: string): void {
    this.element.dispatchEvent(
      new CustomEvent('nav-click', {
        detail: { path },
        bubbles: true,
        composed: true,
      })
    );
  }

  private handleRailKeydown(event: KeyboardEvent): void {
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    const buttons = [...this.railList.querySelectorAll<HTMLButtonElement>('.terminal-rail-select')];
    if (!buttons.length) return;
    event.preventDefault();
    const active = document.activeElement;
    const current = active instanceof HTMLButtonElement ? buttons.indexOf(active) : -1;
    let next = 0;
    if (event.key === 'End') next = buttons.length - 1;
    else if (event.key === 'ArrowDown') next = Math.min(buttons.length - 1, current + 1);
    else if (event.key === 'ArrowUp') next = current <= 0 ? 0 : current - 1;
    buttons[next]?.focus();
  }

  /**
   * Install drag-over / drag-leave / drop handlers on a slot element.
   * Replaces existing handlers on each refresh to capture current preset/index.
   */
  private installDropHandlers(el: HTMLElement, slotIndex: number, preset: TerminalLayout): void {
    // Use a stored handler key to avoid duplicate listeners
    const existing = (el as HTMLElement & { __dropCleanup?: () => void }).__dropCleanup;
    existing?.();

    const onDragOver = (e: DragEvent): void => {
      if (!e.dataTransfer?.types.includes(TERMINAL_DRAG_MIME)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      el.dataset.dragOver = 'true';
    };
    const onDragLeave = (): void => {
      delete el.dataset.dragOver;
    };
    const onDrop = (e: DragEvent): void => {
      delete el.dataset.dragOver;
      if (!e.dataTransfer?.types.includes(TERMINAL_DRAG_MIME)) return;
      e.preventDefault();
      e.stopPropagation();
      const sessionKey = e.dataTransfer.getData(TERMINAL_DRAG_MIME);
      if (!sessionKey || !this.panes.has(sessionKey)) return;
      this.layoutManager.place(sessionKey, preset, slotIndex);
      this.announceResult(sessionKey, slotIndex);
    };

    el.addEventListener('dragover', onDragOver);
    el.addEventListener('dragleave', onDragLeave);
    el.addEventListener('drop', onDrop);

    (el as HTMLElement & { __dropCleanup?: () => void }).__dropCleanup = (): void => {
      el.removeEventListener('dragover', onDragOver);
      el.removeEventListener('dragleave', onDragLeave);
      el.removeEventListener('drop', onDrop);
    };
  }

  /** Announce placement result via aria-live region. */
  private announceResult(sessionKey: string, slotIndex: number): void {
    const entry = this.entries.get(sessionKey);
    const name = entry ? entry.metadata.agent?.name || entry.state.agentId : sessionKey;
    this.ariaLive.textContent = `Placed ${name} in slot ${slotIndex + 1}`;
    // Clear after a delay so repeated placements are announced
    setTimeout(() => {
      if (this.ariaLive.textContent?.includes(name)) {
        this.ariaLive.textContent = '';
      }
    }, 3000);
  }

  /** Open the "Place in pane…" menu, positioned near the trigger button. */
  private openPlaceMenu(sessionKey: string, _agentName: string, trigger: HTMLElement): void {
    const layoutState = this.layoutManager.getState();
    const preset = layoutState.active;
    const slots = SLOT_PLACEMENTS[preset];

    // Build menu items for each slot
    this.placeMenu.replaceChildren();
    const presetSlots = this.getActivePresetSlots(preset);

    for (let i = 0; i < slots.length; i++) {
      const occupant = presetSlots[i];
      const occupantEntry = occupant ? this.entries.get(occupant) : null;
      const occupantName = occupantEntry
        ? occupantEntry.metadata.agent?.name || occupantEntry.state.agentId
        : null;
      const label = occupant
        ? `Slot ${i + 1} (${occupantName ?? occupant})`
        : `Slot ${i + 1} (empty)`;

      const menuItem = document.createElement('button');
      menuItem.type = 'button';
      menuItem.className = 'terminal-place-menu-item';
      menuItem.setAttribute('role', 'menuitem');
      menuItem.textContent = label;
      menuItem.dataset.slotIndex = String(i);
      menuItem.addEventListener('click', (e) => {
        e.stopPropagation();
        this.layoutManager.place(sessionKey, preset, i);
        this.announceResult(sessionKey, i);
        this.closePlaceMenu();
      });
      this.placeMenu.appendChild(menuItem);
    }

    // Position the menu near the trigger
    const rect = trigger.getBoundingClientRect();
    this.placeMenu.style.top = `${rect.bottom + 4}px`;
    this.placeMenu.style.left = `${rect.left}px`;
    this.placeMenu.hidden = false;

    // Focus the first menu item
    requestAnimationFrame(() => {
      const firstItem = this.placeMenu.querySelector<HTMLButtonElement>(
        '.terminal-place-menu-item'
      );
      firstItem?.focus();
    });
  }

  /** Close the "Place in pane…" menu. */
  private closePlaceMenu(): void {
    this.placeMenu.hidden = true;
    this.placeMenu.replaceChildren();
  }

  /** Keyboard navigation within the place menu. */
  private handlePlaceMenuKeydown(event: KeyboardEvent): void {
    const items = [
      ...this.placeMenu.querySelectorAll<HTMLButtonElement>('.terminal-place-menu-item'),
    ];
    if (!items.length) return;

    if (event.key === 'Escape') {
      event.preventDefault();
      this.closePlaceMenu();
      return;
    }

    const active = document.activeElement as HTMLElement;
    const current = items.indexOf(active as HTMLButtonElement);

    if (event.key === 'ArrowDown') {
      event.preventDefault();
      const next = current < items.length - 1 ? current + 1 : 0;
      items[next]?.focus();
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      const next = current > 0 ? current - 1 : items.length - 1;
      items[next]?.focus();
    }
  }

  private publishCount(): void {
    window.dispatchEvent(
      new CustomEvent<TerminalSessionCountDetail>(TERMINAL_SESSION_COUNT_EVENT, {
        detail: { count: this.entries.size },
      })
    );
  }

  // ── URL layout sync (#1715) ─────────────────────────────────────────────

  /**
   * Map session keys in the active preset to agent IDs.
   * Returns an array of agent IDs (or null for empty slots) matching the
   * active preset's slot order.
   */
  getActiveSlotAgentIds(): ReadonlyArray<string | null> {
    const state = this.layoutManager.getState();
    const slots = this.getActivePresetSlots(state.active);
    return slots.map((key) => {
      if (!key) return null;
      const entry = this.entries.get(key);
      return entry?.state.agentId ?? null;
    });
  }

  /**
   * Find the session key for a given agent ID, if one exists.
   * Returns null if no session is found for that agent.
   */
  findSessionKeyByAgentId(agentId: string): string | null {
    for (const [key, entry] of this.entries) {
      if (entry.state.agentId === agentId) return key;
    }
    return null;
  }

  /**
   * Update the browser URL to reflect the current layout state.
   * Uses replaceState (does not create a history entry).
   * Skipped when suppressUrlSync is true (during programmatic restore).
   *
   * Layout query params are only added for multi-pane presets. In single
   * mode the path `/terminals/{agentId}` is sufficient and cleaner — this
   * preserves backward-compatible URLs for the common case.
   */
  syncUrlFromLayout(): void {
    if (this.suppressUrlSync) return;
    const state = this.layoutManager.getState();
    const url = new URL(window.location.href);

    // Helper: clear any existing layout params
    const clearLayoutParams = (): void => {
      for (const key of [...url.searchParams.keys()]) {
        if (key === 'lv' || key === 'lp' || /^s\d+$/.test(key)) {
          url.searchParams.delete(key);
        }
      }
    };

    if (state.active === 'single') {
      // In single mode, clear layout params — the path is enough.
      clearLayoutParams();
    } else {
      // Multi-pane: encode layout + slot agent IDs.
      clearLayoutParams();
      const agentIds = this.getActiveSlotAgentIds();
      const params = serializeLayoutUrl(state.active, agentIds);
      for (const [key, value] of params) {
        url.searchParams.set(key, value);
      }
    }

    const newUrl = url.pathname + url.search;
    // Only update if URL actually changed
    if (newUrl !== window.location.pathname + window.location.search) {
      window.history.replaceState(window.history.state, '', newUrl);
    }
  }

  /**
   * Set the suppressUrlSync flag. Used by external callers (e.g. main.ts)
   * during URL-driven layout restoration to avoid feedback loops.
   */
  setSuppressUrlSync(suppress: boolean): void {
    this.suppressUrlSync = suppress;
  }

  private installStyles(): void {
    const style = document.createElement('style');
    style.textContent = `
      #terminal-workspace {
        background: var(--scion-bg, #f8fafc);
        color: var(--scion-text, #1e293b);
      }
      /* Restore header padding that the global '* { padding: 0 }' reset
         strips when scion-header lives in light DOM (not inside a shadow
         root like app-shell / chat-shell). */
      #terminal-workspace > scion-header {
        padding-inline: 1.5rem;
      }
      .terminal-workspace-shell {
        flex: 1;
        min-height: 0;
        display: grid;
        grid-template-columns: minmax(220px, 280px) minmax(0, 1fr);
        grid-template-rows: minmax(0, 1fr);
      }
      .terminal-rail {
        min-width: 0;
        min-height: 0;
        overflow: hidden;
        border-right: 1px solid var(--scion-border, #e2e8f0);
        background: var(--scion-surface, #fff);
        display: flex;
        flex-direction: column;
      }
      .terminal-rail-header {
        display: flex;
        align-items: center;
        justify-content: space-between;
        padding: 0.875rem 1rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
      }
      .terminal-rail-header h2 {
        margin: 0;
        font-size: 0.875rem;
        font-weight: 650;
      }
      .terminal-count {
        min-width: 1.5rem;
        text-align: center;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }
      .terminal-sort-dropdown {
        margin-left: auto;
      }
      .terminal-sort-btn {
        font-size: 1rem;
        color: var(--scion-text-muted, #64748b);
      }
      .terminal-sort-btn:hover {
        color: var(--scion-text, #1e293b);
      }
      /* Inset to line each button up with its row column (see .terminal-rail-actions). */
      .terminal-rail-bulk {
        display: flex;
        align-items: center;
        justify-content: flex-end;
        gap: 0.125rem;
        padding: 0.375rem 0.75rem 0 0.75rem;
      }
      .terminal-rail-bulk[hidden] {
        display: none;
      }
      .terminal-rail-bulk-label {
        margin-right: auto;
        font-size: 0.6875rem;
        font-weight: 600;
        letter-spacing: 0.02em;
        text-transform: uppercase;
        color: var(--scion-text-muted, #64748b);
      }
      .terminal-bulk-action {
        width: 1.875rem;
        height: 1.625rem;
        display: inline-flex;
        align-items: center;
        justify-content: center;
        border: 1px solid var(--scion-border, #e2e8f0);
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-primary, #3b82f6);
        border-radius: 999px;
        cursor: pointer;
      }
      .terminal-bulk-remove {
        color: var(--scion-status-danger, #ef4444);
      }
      .terminal-bulk-action:hover:not([aria-disabled='true']),
      .terminal-bulk-action:focus-visible {
        border-color: currentColor;
        outline: none;
      }
      /* Nothing to act on: dimmed, dashed and flat, so it does not read as
         a live control in either theme. Still focusable (see
         buildRailBulkActions), so focus keeps a visible ring. */
      .terminal-bulk-action[aria-disabled='true'] {
        cursor: not-allowed;
        color: var(--scion-text-muted, #64748b);
        background: transparent;
        border-style: dashed;
        border-color: var(--scion-text-muted, #64748b);
        opacity: 0.5;
      }
      .terminal-bulk-action[aria-disabled='true']:focus-visible {
        outline: 2px solid var(--scion-text-muted, #64748b);
        outline-offset: 1px;
      }
      .terminal-bulk-action-wrap {
        display: inline-flex;
      }
      /* Shoelace's default tooltip colours resolve to the same neutral in
         the dark theme, so set them from the theme's text and background;
         see ptone/scion#3715. */
      .terminal-bulk-tooltip {
        --sl-tooltip-background-color: var(--scion-text, #1e293b);
        --sl-tooltip-color: var(--scion-bg, #f8fafc);
      }
      .terminal-rail-list {
        flex: 1;
        min-height: 0;
        overflow: auto;
        padding: 0.375rem;
      }
      /* Pinned below the list: only .terminal-rail-list scrolls. */
      .terminal-rail-footer {
        flex: 0 0 auto;
        padding: 0.375rem;
        border-top: 1px solid var(--scion-border, #e2e8f0);
        background: var(--scion-surface, #fff);
      }
      .terminal-jump-btn {
        width: 100%;
        min-height: 2.5rem;
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0 0.625rem;
        border: 0;
        border-radius: 6px;
        background: transparent;
        color: var(--scion-text, #1e293b);
        font: inherit;
        font-size: 0.875rem;
        font-weight: 550;
        text-align: left;
        cursor: pointer;
      }
      .terminal-jump-btn sl-icon {
        flex: 0 0 auto;
        font-size: 1.25rem;
        color: var(--scion-text-muted, #64748b);
      }
      .terminal-jump-label {
        flex: 1;
        min-width: 0;
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .terminal-jump-shortcut {
        flex: 0 0 auto;
        font-family: inherit;
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }
      .terminal-jump-btn:focus-visible {
        background: var(--scion-bg-subtle, #f1f5f9);
        outline: 2px solid var(--scion-primary, #3b82f6);
        outline-offset: -2px;
      }
      /* Hover only where it does not stick after a tap. */
      @media (hover: hover) {
        .terminal-jump-btn:hover {
          background: var(--scion-bg-subtle, #f1f5f9);
        }
      }
      /* Touch: a 44px tap target, and no keyboard-shortcut hint. */
      @media ${TOUCH_PRIMARY_QUERY} {
        .terminal-jump-btn {
          min-height: 44px;
        }
        .terminal-jump-shortcut {
          display: none;
        }
      }
      .terminal-rail-item {
        display: grid;
        grid-template-columns: auto minmax(0, 1fr) auto;
        align-items: center;
        gap: 0.25rem;
        border-radius: 6px;
      }
      .terminal-rail-item[data-dragging='true'] {
        opacity: 0.5;
      }
      .terminal-drag-handle {
        cursor: grab;
        padding: 0.375rem 0.125rem 0.375rem 0.375rem;
        color: var(--scion-text-muted, #64748b);
        font-size: 1rem;
        line-height: 1;
        user-select: none;
        -webkit-user-select: none;
      }
      .terminal-drag-handle:active {
        cursor: grabbing;
      }
      .terminal-rail-item[data-selected='true'] {
        background: color-mix(in srgb, var(--scion-primary, #3b82f6) 10%, transparent);
      }
      .terminal-rail-select {
        min-width: 0;
        display: flex;
        align-items: flex-start;
        gap: 0.625rem;
        border: 0;
        background: transparent;
        color: inherit;
        text-align: left;
        padding: 0.625rem;
        cursor: pointer;
      }
      .terminal-rail-select:hover,
      .terminal-rail-select:focus-visible {
        outline: none;
        background: var(--scion-bg-subtle, #f1f5f9);
        border-radius: 6px;
      }
      .terminal-connection-dot {
        width: 0.625rem;
        height: 0.625rem;
        border-radius: 50%;
        margin-top: 0.25rem;
        background: #94a3b8;
        flex: 0 0 auto;
      }
      .terminal-rail-item[data-connection='connected'] .terminal-connection-dot {
        background: #22c55e;
      }
      .terminal-rail-item[data-connection='disconnected'] .terminal-connection-dot,
      .terminal-rail-item[data-availability='deleted'] .terminal-connection-dot,
      .terminal-rail-item[data-availability='unavailable'] .terminal-connection-dot {
        background: #ef4444;
      }
      .terminal-rail-item[data-connection='loading'] .terminal-connection-dot,
      .terminal-rail-item[data-connection='connecting'] .terminal-connection-dot {
        background: #f59e0b;
      }
      .terminal-rail-text {
        min-width: 0;
        display: flex;
        flex-direction: column;
        gap: 0.125rem;
      }
      .terminal-agent-name,
      .terminal-project-name {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .terminal-agent-name {
        font-size: 0.875rem;
        font-weight: 600;
      }
      .terminal-project-name {
        font-size: 0.75rem;
        color: var(--scion-text-muted, #64748b);
      }
      .terminal-rail-actions {
        display: inline-flex;
        align-items: center;
        gap: 0.125rem;
        padding-right: 0.375rem;
      }
      .terminal-icon-action {
        width: 1.875rem;
        height: 1.875rem;
        display: inline-flex;
        align-items: center;
        justify-content: center;
        border: 0;
        background: transparent;
        color: var(--scion-text-muted, #64748b);
        border-radius: 4px;
        cursor: pointer;
      }
      .terminal-icon-action:hover,
      .terminal-icon-action:focus-visible {
        color: var(--scion-text, #1e293b);
        background: var(--scion-bg-subtle, #f1f5f9);
        outline: none;
      }
      .terminal-icon-action:disabled {
        cursor: default;
        opacity: 0.35;
      }
      .terminal-pane-area {
        display: flex;
        flex-direction: column;
        min-width: 0;
        min-height: 0;
      }
      .terminal-layout-bar {
        display: flex;
        align-items: center;
        gap: 0.5rem;
        padding: 0.375rem 0.75rem;
        border-bottom: 1px solid var(--scion-border, #e2e8f0);
        background: var(--scion-surface, #fff);
        flex: 0 0 auto;
      }
      .terminal-layout-label {
        font-size: 0.75rem;
        font-weight: 600;
        color: var(--scion-text-muted, #64748b);
        margin-right: 0.25rem;
      }
      .terminal-layout-btn {
        font-size: 0.75rem;
        padding: 0.25rem 0.5rem;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: 4px;
        background: transparent;
        color: var(--scion-text-muted, #64748b);
        cursor: pointer;
        white-space: nowrap;
      }
      .terminal-layout-btn:hover,
      .terminal-layout-btn:focus-visible {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text, #1e293b);
        outline: none;
      }
      .terminal-layout-btn[data-active='true'] {
        background: color-mix(in srgb, var(--scion-primary, #3b82f6) 15%, transparent);
        border-color: var(--scion-primary, #3b82f6);
        color: var(--scion-primary, #3b82f6);
        font-weight: 600;
      }
      .terminal-layout-restore {
        font-size: 0.75rem;
        padding: 0.25rem 0.5rem;
        margin-left: auto;
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: 4px;
        background: transparent;
        color: var(--scion-text-muted, #64748b);
        cursor: pointer;
      }
      .terminal-layout-restore:hover,
      .terminal-layout-restore:focus-visible {
        background: var(--scion-bg-subtle, #f1f5f9);
        color: var(--scion-text, #1e293b);
        outline: none;
      }
      .terminal-pane-host {
        position: relative;
        min-width: 0;
        min-height: 0;
        flex: 1;
        display: grid;
        grid-template-columns: 1fr;
        grid-template-rows: 1fr;
        background: #111827;
      }
      .terminal-pane {
        min-height: 0;
        min-width: 0;
        border: 1px solid #333;
        border-radius: 4px;
      }
      /* Focus outline only in multi-pane layouts — in single-pane mode focus
         is implicit and the outline is visual noise (#1716). */
      .terminal-pane-host[data-effective-layout='two-columns'] scion-terminal-pane[data-focused],
      .terminal-pane-host[data-effective-layout='two-rows'] scion-terminal-pane[data-focused],
      .terminal-pane-host[data-effective-layout='four'] scion-terminal-pane[data-focused] {
        outline: 2px solid var(--scion-primary, #3b82f6);
        outline-offset: -2px;
        border-color: var(--scion-primary, #3b82f6);
      }
      .terminal-slot-placeholder {
        display: flex;
        align-items: center;
        justify-content: center;
        border: 2px dashed var(--scion-border, #e2e8f0);
        border-radius: 8px;
        margin: 4px;
        color: var(--scion-text-muted, #64748b);
        font-size: 0.875rem;
        background: color-mix(in srgb, var(--scion-bg, #f8fafc) 50%, transparent);
        min-height: 0;
        min-width: 0;
        transition: border-color 0.15s, background 0.15s;
      }
      .terminal-slot-placeholder[data-drag-over='true'] {
        border-color: var(--scion-primary, #3b82f6);
        background: color-mix(in srgb, var(--scion-primary, #3b82f6) 15%, transparent);
      }
      scion-terminal-pane[data-drag-over='true'] {
        outline: 2px solid var(--scion-primary, #3b82f6);
        outline-offset: -2px;
      }
      .terminal-aria-live {
        position: absolute;
        width: 1px;
        height: 1px;
        overflow: hidden;
        clip: rect(0, 0, 0, 0);
        white-space: nowrap;
      }
      .terminal-place-menu {
        position: fixed;
        z-index: 1000;
        min-width: 12rem;
        background: var(--scion-surface, #fff);
        border: 1px solid var(--scion-border, #e2e8f0);
        border-radius: 6px;
        box-shadow: 0 4px 12px rgba(0,0,0,0.1);
        padding: 0.25rem;
      }
      .terminal-place-menu-item {
        display: block;
        width: 100%;
        text-align: left;
        padding: 0.5rem 0.75rem;
        border: 0;
        background: transparent;
        color: var(--scion-text, #1e293b);
        font-size: 0.8125rem;
        cursor: pointer;
        border-radius: 4px;
      }
      .terminal-place-menu-item:hover,
      .terminal-place-menu-item:focus-visible {
        background: var(--scion-bg-subtle, #f1f5f9);
        outline: none;
      }
      .terminal-empty,
      .terminal-status {
        position: absolute;
        inset: 0;
        display: flex;
        align-items: center;
        justify-content: center;
        margin: 0;
        padding: 2rem;
        color: var(--scion-text-muted, #64748b);
        background: var(--scion-bg, #f8fafc);
        text-align: center;
        z-index: 1;
      }
      #terminal-workspace [hidden] {
        display: none !important;
      }
      @media (max-width: 760px) {
        .terminal-workspace-shell {
          grid-template-columns: 1fr;
          /* 11rem min keeps at least one list row visible between the
             rail header and its pinned Jump to agent footer, capped at
             45% of the shell so short landscape phones keep pane room. */
          grid-template-rows: minmax(min(11rem, 45%), 35vh) minmax(0, 1fr);
        }
        .terminal-rail {
          border-right: 0;
          border-bottom: 1px solid var(--scion-border, #e2e8f0);
        }
      }
    `;
    this.element.appendChild(style);
  }
}

function countLabel(count: number): string {
  return `${count} ${count === 1 ? 'terminal' : 'terminals'}`;
}

function connectionLabel(state: TerminalConnectionState): string {
  switch (state) {
    case 'idle':
      return 'Not connected';
    case 'loading':
      return 'Pending';
    case 'connecting':
      return 'Connecting';
    case 'connected':
      return 'Connected';
    case 'disconnected':
      return 'Disconnected';
    case 'unavailable':
      return 'Unavailable';
    case 'closed':
      return 'Closed';
  }
}

function disconnectLabel(state: TerminalConnectionState, reason: TerminalDisconnectReason): string {
  if (
    state === 'connected' ||
    state === 'loading' ||
    state === 'connecting' ||
    state === 'closed' ||
    state === 'idle'
  )
    return connectionLabel(state);
  switch (reason) {
    case 'auth-401':
      return 'Auth required';
    case 'auth-403':
      return 'Access denied';
    case 'not-found':
      return 'Not found';
    case 'session-ended':
      return 'Session ended';
    case 'detached':
      return 'Detached';
    case 'agent-offline':
    case 'agent-phase':
    case 'agent-stopped':
      return 'Unavailable';
    case 'agent-deleted':
      return 'Deleted';
    case 'network':
    case 'connect-error':
    case 'server-error':
      return 'Disconnected';
    default:
      return connectionLabel(state);
  }
}

function availabilityLabel(availability: TerminalAgentMetadata['availability']): string {
  switch (availability) {
    case 'loading':
      return 'metadata pending';
    case 'ready':
      return 'agent available';
    case 'deleted':
      return 'agent deleted';
    case 'unavailable':
      return 'metadata unavailable';
  }
}
