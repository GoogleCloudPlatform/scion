// Isolated production-pane fixture: network/clipboard boundaries are supplied by Playwright.
import { TerminalSessionRegistry } from '../../src/client/terminal-sessions.js';
import '../../src/components/terminal/terminal-pane.js';
import '@shoelace-style/shoelace/dist/components/dialog/dialog.js';
import '@shoelace-style/shoelace/dist/components/button/button.js';
import '@shoelace-style/shoelace/dist/components/radio/radio.js';
import '@shoelace-style/shoelace/dist/components/radio-group/radio-group.js';
import '@shoelace-style/shoelace/dist/themes/dark.css';
import type { Terminal } from '@xterm/xterm';

const pane = document.createElement('scion-terminal-pane');
const registry = new TerminalSessionRegistry({
  hubUrl: location.origin,
  accountId: 'fixture-account',
});
if (location.search.includes('hidden')) pane.setVisible(false);
document.body.append(pane);
const session = pane.open(registry, '11111111-1111-4111-8111-111111111111');
// Test-only inspection; no production API exposes xterm internals.
const terminal = (): Terminal => (pane as unknown as { terminal: Terminal }).terminal;
let remembered: [Terminal, HTMLElement | undefined] | undefined;
let disposals = 0;
const fixture = {
  pane,
  session,
  terminal,
  remember(): void {
    remembered = [terminal(), terminal().element];
    const original = terminal().dispose.bind(terminal());
    terminal().dispose = (): void => {
      disposals++;
      original();
    };
  },
  sameIdentity(): boolean {
    return remembered?.[0] === terminal() && remembered?.[1] === terminal().element;
  },
  get disposals(): number {
    return disposals;
  },
  text(): string {
    const buffer = terminal().buffer.active;
    return Array.from(
      { length: buffer.length },
      (_, index) => buffer.getLine(index)?.translateToString(true) ?? ''
    ).join('\n');
  },
};
window.paneFixture = fixture;
declare global {
  interface Window {
    paneFixture: typeof fixture;
    clipboardText: string;
  }
}
