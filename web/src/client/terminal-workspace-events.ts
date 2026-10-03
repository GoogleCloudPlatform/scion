export interface TerminalSessionCountDetail {
  readonly count: number;
}

export const TERMINAL_SESSION_COUNT_EVENT = 'scion:terminal-session-count';

/** Custom MIME type for terminal drag payloads. */
export const TERMINAL_DRAG_MIME = 'application/x-scion-terminal';

/**
 * Detail for {@link TERMINAL_PALETTE_NEW_AGENT_EVENT}: an agent the "Jump to
 * agent" palette selected that has no session yet in this tab's registry, so
 * a brand new one must be created through the coordinator (cross-tab
 * ownership/session-reuse) — unlike an already-open agent, which
 * `TerminalWorkspaceRoot` places directly with no coordinator round-trip.
 */
export interface TerminalPaletteNewAgentDetail {
  readonly agentId: string;
}

/**
 * Dispatched by `TerminalWorkspaceRoot` when the palette selects an agent
 * with no existing session while more than one pane is on screen. `main.ts`
 * is the only listener: it alone holds the module-local
 * `TerminalCoordinator` reference a brand new session must go through (see
 * `TerminalWorkspaceRoot.create`'s own doc comment for why placement itself
 * still happens inside `create()`, not here).
 */
export const TERMINAL_PALETTE_NEW_AGENT_EVENT = 'scion:terminal-palette-new-agent';
