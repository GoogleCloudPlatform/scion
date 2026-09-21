export interface TerminalSessionCountDetail {
  readonly count: number;
}

export const TERMINAL_SESSION_COUNT_EVENT = 'scion:terminal-session-count';

/** Custom MIME type for terminal drag payloads. */
export const TERMINAL_DRAG_MIME = 'application/x-scion-terminal';
