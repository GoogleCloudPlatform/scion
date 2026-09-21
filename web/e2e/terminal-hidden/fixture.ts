/**
 * Isolated fixture for hidden terminal interaction tests (P1.8).
 *
 * Network boundaries (HTTP, WebSocket, EventSource, clipboard) are supplied
 * by Playwright — no live Hub, no desktop clipboard. The fixture exposes
 * gated async helpers so tests can control ordering of paste, upload and
 * clipboard completions relative to hide/show transitions.
 */
import { TerminalSessionRegistry } from '../../src/client/terminal-sessions.js';
import '../../src/components/terminal/terminal-pane.js';
import '@shoelace-style/shoelace/dist/components/dialog/dialog.js';
import '@shoelace-style/shoelace/dist/components/button/button.js';
import '@shoelace-style/shoelace/dist/themes/dark.css';
import type { Terminal } from '@xterm/xterm';

const agentId = '11111111-1111-4111-8111-111111111111';

const pane = document.createElement('scion-terminal-pane');
const registry = new TerminalSessionRegistry({
  hubUrl: location.origin,
  accountId: 'fixture-account',
});
document.body.append(pane);
const session = pane.open(registry, agentId);

// Expose xterm internals for test-only inspection.
const terminal = (): Terminal => (pane as unknown as { terminal: Terminal }).terminal;

// Gated clipboard mock: tests resolve the readText promise manually.
let clipboardText = '';
let clipboardReadResolve: ((text: string) => void) | null = null;

Object.defineProperty(navigator, 'clipboard', {
  value: {
    writeText: (text: string): Promise<void> => {
      clipboardText = text;
      return Promise.resolve();
    },
    readText: (): Promise<string> => {
      if (clipboardReadResolve) {
        // Gated mode: return a promise the test controls.
        return new Promise<string>((resolve) => {
          clipboardReadResolve = resolve;
        });
      }
      return Promise.resolve(clipboardText);
    },
  },
});

const fixture = {
  pane,
  session,
  terminal,
  get clipboardText(): string {
    return clipboardText;
  },
  set clipboardText(value: string) {
    clipboardText = value;
  },
  /** Enable gated clipboard mode — readText won't resolve until releaseClipboard is called. */
  gateClipboard(): void {
    clipboardReadResolve = (): void => {};
  },
  /** Resolve the pending gated readText promise. */
  releaseClipboard(text: string): void {
    clipboardReadResolve?.(text);
    clipboardReadResolve = null;
  },
  text(): string {
    const t = terminal();
    if (!t) return '';
    const buffer = t.buffer.active;
    return Array.from(
      { length: buffer.length },
      (_, index) => buffer.getLine(index)?.translateToString(true) ?? ''
    ).join('\n');
  },
};

window.hiddenFixture = fixture;

// Wire up the drop target to record dropped files.
const dropTarget = document.getElementById('drop-target')!;
dropTarget.addEventListener('dragover', (e) => {
  e.preventDefault();
  if (e.dataTransfer) e.dataTransfer.dropEffect = 'copy';
});
dropTarget.addEventListener('drop', (e) => {
  e.preventDefault();
  const files = e.dataTransfer?.files;
  if (files?.length) {
    dropTarget.textContent = `Received: ${Array.from(files)
      .map((f) => f.name)
      .join(', ')}`;
    dropTarget.dataset.dropped = 'true';
  }
});

declare global {
  interface Window {
    hiddenFixture: typeof fixture;
  }
}
