/**
 * Isolated fixture for the native chat quick command palette (Phase 1: the
 * Agents/DM slice). Mounts the real `scion-page-chat` (which lazy-loads the
 * real `scion-chat-switcher`) and, on demand, a real `scion-terminal-pane`
 * with a real xterm — network/SSE/clipboard boundaries are supplied by
 * Playwright's request interception, not by a production mock mode.
 *
 * The chat outlet/terminal outlet split mirrors main.ts: the terminal
 * workspace hides the route outlet (not the page itself) while chat stays
 * mounted underneath — see `hideChatShowTerminal`.
 */
import type { PageData } from '../../src/shared/types.js';
import '../../src/components/pages/chat.js';
import '../../src/components/terminal/terminal-pane.js';
import { TerminalSessionRegistry } from '../../src/client/terminal-sessions.js';
// This fixture stubs out client/main.ts entirely (see mock-api.ts:
// stubMainClientModule) to avoid its real app bootstrap, which also means
// its bulk Shoelace component registration never runs. Mirror that exact
// list here so every Shoelace tag the real chat/composer/thread templates
// use is actually defined (main.ts's own import block, kept in sync by eye).
import '@shoelace-style/shoelace/dist/components/breadcrumb/breadcrumb.js';
import '@shoelace-style/shoelace/dist/components/breadcrumb-item/breadcrumb-item.js';
import '@shoelace-style/shoelace/dist/components/button/button.js';
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
import '@shoelace-style/shoelace/dist/themes/light.css';

// The router never runs in this fixture, so set the URL by hand before the
// page's connectedCallback reads window.location.pathname for route parsing
// and the shortcut's route guard. A test that needs to start inside an
// existing DM (composer already mounted) navigates to
// fixture.html?route=%2Fchat%2Fdm%2F... instead of the bare /chat default.
const params = new URLSearchParams(location.search);
window.history.replaceState({}, '', params.get('route') || '/chat');

// Server-injected feature flags, exactly as main.ts would set them from the
// Go template. Both default on for this fixture; a test that needs the
// "flag off" case (the legacy switcher, or v1, remaining unaffected)
// navigates to fixture.html?v2=0 or ?palette=0 instead — isV2 is captured
// once at construction, so it must be set before the page element is
// created.
window.__SCION_FEATURES__ = {
  'web.native_chat_v2': params.get('v2') !== '0',
  'web.native_chat_palette': params.get('palette') !== '0',
};

const TEST_USER_ID = 'self-user';
// Kept in sync by eye with mock-api.ts's TERMINAL_AGENT_ID — that file is
// Playwright-only (imports `@playwright/test`'s types) and this one loads in
// the real browser, so they can't share the constant directly.
const AGENT_ID = '11111111-1111-4111-8111-111111111111';
export const FIXTURE_AGENT_ID = AGENT_ID;
export const FIXTURE_USER_ID = TEST_USER_ID;

const pageData: PageData = {
  path: '/chat',
  title: 'Chat',
  user: { id: TEST_USER_ID, email: 'self@example.com', name: 'Self User' },
};

const chatOutlet = document.getElementById('chat-outlet')!;
const terminalOutlet = document.getElementById('terminal-outlet')!;

const page = document.createElement('scion-page-chat') as HTMLElement & { pageData: PageData };
page.pageData = pageData;
chatOutlet.appendChild(page);

const terminalRegistry = new TerminalSessionRegistry({
  hubUrl: location.origin,
  accountId: 'fixture-account',
});
let terminalPane: HTMLElement | null = null;
// Exposed so a test can wait for the fixture's own real attach flow (agent
// fetch, preflight, WebSocket) to reach 'connected' before typing — mirrors
// e2e/terminal-pane's fixture.ts `session` field.
let terminalSession: { state: { connection: string } } | null = null;

const fixture = {
  page,

  /**
   * Reflects `renderRoute`'s real terminal-workspace transition in main.ts:
   * the URL moves to /terminals/<id> *and* the route outlet is hidden — not
   * scion-page-chat itself, which stays mounted underneath. Both halves
   * matter: the route guard and the visibility guard are two separate
   * checks, and a test that only flips one of them isn't exercising the
   * other.
   */
  hideChatShowTerminal(): void {
    window.history.pushState({}, '', `/terminals/${AGENT_ID}`);
    chatOutlet.hidden = true;
    terminalOutlet.hidden = false;
    if (!terminalPane) {
      terminalPane = document.createElement('scion-terminal-pane');
      terminalOutlet.appendChild(terminalPane);
      terminalSession = (
        terminalPane as unknown as {
          open: (r: typeof terminalRegistry, id: string) => { state: { connection: string } };
        }
      ).open(terminalRegistry, AGENT_ID);
    }
  },

  showChatHideTerminal(): void {
    window.history.pushState({}, '', '/chat');
    chatOutlet.hidden = false;
    terminalOutlet.hidden = true;
  },

  /**
   * Mounts a real terminal pane *alongside* chat — route stays `/chat` and
   * the chat outlet stays visible, unlike `hideChatShowTerminal`. This
   * isolates the terminal-surface (composedPath) guard from the route and
   * visibility guards: a Ctrl+K with focus inside this terminal must still
   * be swallowed even though the page is otherwise fully on-route and
   * visible.
   */
  showTerminalAlongsideChat(): void {
    terminalOutlet.hidden = false;
    if (!terminalPane) {
      terminalPane = document.createElement('scion-terminal-pane');
      terminalOutlet.appendChild(terminalPane);
      terminalSession = (
        terminalPane as unknown as {
          open: (r: typeof terminalRegistry, id: string) => { state: { connection: string } };
        }
      ).open(terminalRegistry, AGENT_ID);
    }
  },

  terminalConnection(): string | null {
    return terminalSession?.state.connection ?? null;
  },
};

declare global {
  interface Window {
    chatPaletteFixture: typeof fixture;
  }
}
window.chatPaletteFixture = fixture;
