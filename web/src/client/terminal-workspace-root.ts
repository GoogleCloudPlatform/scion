import type { User } from '../shared/types.js';
import {
  TerminalSessionRegistry,
  type TerminalConnectionState,
  type TerminalSession,
  type TerminalSessionState,
  type TerminalDisconnectReason,
} from './terminal-sessions.js';
import {
  TerminalLayoutManager,
  type TerminalLayout,
  type TerminalSlot,
} from './terminal-layout.js';
import type { TerminalAgentMetadata } from './terminal-metadata.js';
import type { ScionHeader } from '../components/shared/header.js';
import type { ScionTerminalPane } from '../components/terminal/terminal-pane.js';
import {
  TERMINAL_SESSION_COUNT_EVENT,
  TERMINAL_DRAG_MIME,
  type TerminalSessionCountDetail,
} from './terminal-workspace-events.js';
import '../components/shared/header.js';
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
  private readonly count = document.createElement('span');
  private readonly empty = document.createElement('div');
  private readonly layoutBar = document.createElement('div');
  private readonly paneHost = document.createElement('section');
  private readonly status = document.createElement('p');
  private readonly panes = new Map<string, ScionTerminalPane>();
  private readonly entries = new Map<string, RailEntry>();
  private readonly placeholders = new Map<number, HTMLElement>();
  private readonly ariaLive = document.createElement('div');
  private readonly placeMenu = document.createElement('div');
  readonly layoutManager = new TerminalLayoutManager();
  private registryUnsubscribe: (() => void) | null = null;
  private registry: TerminalSessionRegistry | null = null;
  private currentPath = '/terminals';
  private refreshQueued = false;
  private narrowQuery: MediaQueryList | null = null;
  /** Monotonic counter for stable chronological rail ordering. */
  private entryCounter = 0;
  /** Current rail sort mode. */
  private railSort: 'added' | 'alpha' | 'activity' = 'added';

  private user: User | null = null;

  constructor(user: User | null = null) {
    this.user = user;
    this.element.id = 'terminal-workspace';
    this.element.hidden = true;
    this.element.style.cssText = 'height:100vh;min-height:0;display:none;flex-direction:column';
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
    this.rail.append(railHeader, this.railList);

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

    this.shell.append(this.rail, this.createPaneArea());
    this.element.append(this.header, this.shell, this.ariaLive, this.placeMenu);

    // Subscribe to layout state (lives as long as the workspace root)
    this.layoutManager.subscribe(() => this.queueRefresh());

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

  create(registry: TerminalSessionRegistry, agentId: string): TerminalSession {
    this.bindRegistry(registry);
    const pane = document.createElement('scion-terminal-pane');
    pane.className = 'terminal-pane';
    pane.style.cssText = 'height:100%;width:100%;min-height:0';
    pane.userId = this.user?.id ?? '';
    pane.setVisible(false);
    this.paneHost.appendChild(pane);
    try {
      const session = pane.open(registry, agentId);
      this.panes.set(session.state.key, pane);
      return session;
    } catch (error) {
      pane.remove();
      throw error;
    }
  }

  select(session: TerminalSession): void {
    const pane = this.panes.get(session.state.key);
    if (!pane) throw new Error('Terminal session has no retained pane.');
    // Sets single[0] without changing the active preset (#1701).
    this.layoutManager.open(session.state.key);
    this.status.textContent = '';
    this.show(true);
    this.refresh();
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
    this.element.hidden = !visible;
    this.element.style.display = visible ? 'flex' : 'none';
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
            next.agent?.phase === 'stopped' &&
            entry.session.state.connection !== 'closed' &&
            entry.session.state.connection !== 'unavailable'
          ) {
            entry.session.markUnavailable('agent-stopped', 'Agent has stopped.');
          }
        } catch {
          // markUnavailable should not throw, but guard the subscription callback
        }

        this.queueRefresh();
      });
      this.entries.set(session.state.key, entry);
    }
    // If no active session, auto-select via layout manager
    const currentSlots = this.layoutManager.getVisibleSlots();
    if (!currentSlots.some((s) => s !== null) && sessions.length > 0) {
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

    const layoutState = this.layoutManager.getState();
    const visibleSlots = this.layoutManager.getVisibleSlots();
    const hasSelected = visibleSlots.some((s) => s !== null);

    // In multi-pane layouts with zero agents, show dotted placeholders instead
    // of the "No terminals are open." message. This gives the user clear drop
    // targets even before any session has been created.
    const isMultiPane = layoutState.active !== 'single';
    this.empty.hidden = total > 0 || isMultiPane;
    this.status.hidden = (total > 0 && hasSelected) || (total === 0 && isMultiPane);

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

  private renderRailEntry(entry: RailEntry): HTMLElement {
    const metadata = entry.metadata;
    const agent = metadata.agent ?? entry.state.agent;
    const agentName = agent?.name || entry.state.agentId;
    const projectId = agent?.projectId || 'Unknown project';
    const item = document.createElement('div');
    item.className = 'terminal-rail-item';
    item.setAttribute('role', 'listitem');
    // Mark as selected if this key is in the visible slots
    const visibleSlots = this.layoutManager.getVisibleSlots();
    item.dataset.selected = String(visibleSlots.includes(entry.state.key));
    item.dataset.connection = entry.state.connection;
    item.dataset.availability = metadata.availability;
    if (entry.state.disconnectReason) item.dataset.disconnectReason = entry.state.disconnectReason;

    const select = document.createElement('button');
    select.type = 'button';
    select.className = 'terminal-rail-select';
    select.setAttribute('aria-label', `Show terminal for ${agentName} in ${projectId}`);
    if (visibleSlots.includes(entry.state.key)) select.setAttribute('aria-current', 'page');
    select.dataset.railFocusId = `${entry.state.key}:select`;
    select.addEventListener('click', () => this.openSessionRoute(entry));

    const connection = document.createElement('span');
    connection.className = 'terminal-connection-dot';
    connection.title = connectionLabel(entry.state.connection);
    connection.setAttribute('aria-hidden', 'true');
    const text = document.createElement('span');
    text.className = 'terminal-rail-text';
    const name = document.createElement('span');
    name.className = 'terminal-agent-name';
    name.textContent = agentName;
    const project = document.createElement('span');
    project.className = 'terminal-project-name';
    project.textContent = projectId;
    const details = document.createElement('span');
    details.className = 'terminal-state-label';
    details.textContent = `${disconnectLabel(entry.state.connection, entry.state.disconnectReason)} · ${availabilityLabel(
      metadata.availability
    )}`;
    text.append(name, project, details);
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
    reconnect.disabled =
      entry.state.connection === 'loading' ||
      entry.state.connection === 'connecting' ||
      entry.state.connection === 'connected' ||
      entry.state.connection === 'closed' ||
      entry.state.disconnectReason === 'agent-deleted';
    reconnect.addEventListener('click', (event) => {
      event.stopPropagation();
      void this.registry?.metadata.refresh(entry.state.agentId);
      void entry.session.connect();
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
      entry.session.close();
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

  private openSessionRoute(entry: RailEntry): void {
    this.dispatchNavigation(`/terminals/${entry.state.agentId}`);
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
      }
      .terminal-rail {
        min-width: 0;
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
      .terminal-rail-list {
        flex: 1;
        min-height: 0;
        overflow: auto;
        padding: 0.375rem;
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
      .terminal-project-name,
      .terminal-state-label {
        overflow: hidden;
        text-overflow: ellipsis;
        white-space: nowrap;
      }
      .terminal-agent-name {
        font-size: 0.875rem;
        font-weight: 600;
      }
      .terminal-project-name,
      .terminal-state-label {
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
      scion-terminal-pane[data-focused] {
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
          grid-template-rows: minmax(9rem, 35vh) minmax(0, 1fr);
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

function connectionLabel(state: TerminalConnectionState): string {
  switch (state) {
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
  if (state === 'connected' || state === 'loading' || state === 'connecting' || state === 'closed')
    return connectionLabel(state);
  switch (reason) {
    case 'auth-401':
      return 'Auth required';
    case 'auth-403':
      return 'Access denied';
    case 'not-found':
      return 'Not found';
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
