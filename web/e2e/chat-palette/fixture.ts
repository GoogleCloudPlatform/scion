/**
 * Isolated fixture for the native chat quick command palette. Mounts the
 * real `scion-page-chat` (which lazy-loads the
 * real `scion-quick-palette`) and, on demand, a real `scion-terminal-pane`
 * with a real xterm — network/SSE/clipboard boundaries are supplied by
 * Playwright's request interception, not by a production mock mode.
 *
 * The chat outlet/terminal outlet split mirrors main.ts: the terminal
 * workspace hides the route outlet (not the page itself) while chat stays
 * mounted underneath — see `hideChatShowTerminal`.
 */
import type { PageData } from '../../src/shared/types.js';
import '../../src/components/pages/chat.js';
import '../../src/components/chat/chat-shell.js';
import type { ScionChatShell } from '../../src/components/chat/chat-shell.js';
import '../../src/components/terminal/terminal-pane.js';
import { TerminalSessionRegistry } from '../../src/client/terminal-sessions.js';
import { chatRecentFiles } from '../../src/client/chat-recent-files.js';
import { chatUnread, startChatUnreadIfEligible } from '../../src/client/chat-unread.js';
import type { RecentFile } from '../../src/client/chat-recent-files.js';
import { attachmentIdentityKey, pathIdentityKey } from '../../src/utils/chat-file-links.js';
import type { PathLinkTarget } from '../../src/utils/chat-file-links.js';
import { setAnimation } from '@shoelace-style/shoelace/dist/utilities/animation-registry.js';
import { setBasePath } from '@shoelace-style/shoelace/dist/utilities/base-path.js';
// This fixture stubs out client/main.ts entirely (see mock-api.ts's
// stubMainClientModule), which is the only place the real app calls
// setBasePath('/shoelace') — without it, <sl-icon> cannot resolve its SVGs
// and every icon (including the palette button's) renders blank, which
// understates real layout width at narrow viewports.
setBasePath('/shoelace');
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
// Imported after light.css: both stylesheets scope their custom properties
// to `.sl-theme-dark` (dark) vs. `:root`/`.sl-theme-light` (light), which tie
// on specificity for an element carrying both — source order is what makes
// dark actually win once a test adds the `.sl-theme-dark` class. The real
// app's own theme.css (--scion-* tokens, not Shoelace's --sl-* ones) is
// loaded the same way, so an accessibility check against this fixture is
// checking real, currently-shipping colors, not CSS-custom-property
// fallbacks that never render in production.
import '@shoelace-style/shoelace/dist/themes/dark.css';
import '../../src/styles/theme.css';

// theme.css falls back to a `prefers-color-scheme: dark` media query when
// `data-theme` is unset, so the browser/OS's own color-scheme preference
// (not under this fixture's control, and not necessarily light in a headless
// container) would otherwise decide which theme's tokens apply. The real
// app's header.ts never leaves this unset either — it always resolves and
// sets `data-theme` before anything else renders. A test that wants dark
// overrides this explicitly (see accessibility.pw.ts's enableDarkTheme).
document.documentElement.setAttribute('data-theme', 'light');

// The router never runs in this fixture, so set the URL by hand before the
// page's connectedCallback reads window.location.pathname for route parsing
// and the shortcut's route guard. A test that needs to start inside an
// existing DM (composer already mounted) navigates to
// fixture.html?route=%2Fchat%2Fdm%2F... instead of the bare /chat default.
const params = new URLSearchParams(location.search);
window.history.replaceState({}, '', params.get('route') || '/chat');

// `?unread=1` starts the unread conversation counter before the page mounts,
// as main.ts does for a signed-in user with chat enabled whose first route is
// a chat route (every route this fixture serves is one): through
// startChatUnreadIfEligible with the chat-route flag set, so the counter
// asks for the unread count at once, as in the real app.
if (params.get('unread') === '1') startChatUnreadIfEligible(chatUnread, true, true, true);

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

/**
 * `?shell=1` mounts the real `<scion-chat-shell>` (header + slot) with the
 * page slotted inside, exactly as main.ts does for a real chat route — the
 * header renders as the page's sibling, not its ancestor, so this is the
 * only fixture mode that exercises the real, production event path from the
 * header's palette button through to the page's document-level listener.
 * The default (bare `scion-page-chat`, no shell) mode every other spec in
 * this directory uses is unaffected.
 */
const useShell = params.get('shell') === '1';
let chatShell: ScionChatShell | null = null;
if (useShell) {
  chatShell = document.createElement('scion-chat-shell');
  chatShell.user = pageData.user ?? null;
  chatShell.currentPath = window.location.pathname;
  chatShell.appendChild(page);
  chatOutlet.appendChild(chatShell);
  // Mirrors main.ts's router: the shell's own currentPath only reflects the
  // route at mount time otherwise, which would leave the header showing the
  // wrong mode/button after a fixture-driven navigation (e.g.
  // hideChatShowTerminal's pushState).
  window.addEventListener('popstate', () => {
    if (chatShell) chatShell.currentPath = window.location.pathname;
  });
} else {
  chatOutlet.appendChild(page);
}

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

  /**
   * Seed the Documents group's data through the real store: writes a
   * validly-shaped envelope to the real localStorage key, then calls the
   * real `chatRecentFiles.setScope`, which hydrates from it exactly as it
   * would for a real signed-in user. This is a test seam for *which files
   * exist*, not a bypass of the store's own validation/identity logic — a
   * malformed fixture record would be silently dropped by `setScope` the
   * same way a corrupt real record would be.
   */
  seedRecentFiles(inputs: FixtureRecentFileInput[]): void {
    const records = inputs.map(buildRecentFile);
    const scope = {
      origin: location.origin,
      baseUrl: import.meta.env.BASE_URL,
      userId: TEST_USER_ID,
    };
    // Mirrors chat-recent-files.ts's private storageKey() format exactly.
    const key =
      'scion.chat.recentFiles.v1:' + JSON.stringify([scope.origin, scope.baseUrl, scope.userId]);
    localStorage.setItem(key, JSON.stringify({ version: 1, records }));
    chatRecentFiles.setScope(scope);
  },

  /**
   * Override the palette's own dialog hide animation duration, per element
   * (Shoelace's `setAnimation`, not the shared `setDefaultAnimation`), so a
   * reopen-race test can control exactly how long the close stays "still
   * animating out" rather than relying on Shoelace's real 250ms default. The
   * real gap between a selection and the browser actually starting that
   * animation (event dispatch, this app's own close-path work) varies with
   * machine load, so measuring a fixed set of reopen delays from the
   * *selection* — rather than from the animation's own start — cannot
   * otherwise guarantee every delay in an unloaded run still lands before a
   * real 250ms hide has finished. The palette must already be open (so its
   * dialog element exists) before calling this.
   */
  setPaletteDialogHideDuration(ms: number): void {
    const dialog = document
      .querySelector('scion-page-chat')
      ?.shadowRoot?.querySelector('scion-quick-palette')
      ?.shadowRoot?.querySelector('sl-dialog.palette-dialog');
    if (!(dialog instanceof HTMLElement)) {
      throw new Error(
        'setPaletteDialogHideDuration: palette dialog not found — open the palette first'
      );
    }
    setAnimation(dialog, 'dialog.hide', {
      keyframes: [
        { opacity: 1, scale: 1 },
        { opacity: 0, scale: 0.8 },
      ],
      options: { duration: ms, easing: 'ease' },
    });
  },

  /**
   * Same as {@link setPaletteDialogHideDuration}, for the page-level file
   * preview's own dialog instead of the palette's. The preview must already
   * be open (so its dialog element exists) before calling this.
   */
  setFilePreviewDialogHideDuration(ms: number): void {
    const dialog = document
      .querySelector('scion-page-chat')
      ?.shadowRoot?.querySelector('scion-chat-file-preview')
      ?.shadowRoot?.querySelector('sl-dialog.file-preview-dialog');
    if (!(dialog instanceof HTMLElement)) {
      throw new Error(
        'setFilePreviewDialogHideDuration: preview dialog not found — open the preview first'
      );
    }
    setAnimation(dialog, 'dialog.hide', {
      keyframes: [
        { opacity: 1, scale: 1 },
        { opacity: 0, scale: 0.8 },
      ],
      options: { duration: ms, easing: 'ease' },
    });
  },
};

/** One fixture-supplied Documents record, before its stable identity key is computed. */
export interface FixtureRecentFileInput {
  name: string;
  sentAt: string;
  projectId?: string;
  projectName?: string;
  target:
    | { kind: 'attachment'; id: string; mime: string; size: number }
    | { kind: 'path'; projectId: string; containerPath: string; location: PathLinkTarget };
}

function buildRecentFile(input: FixtureRecentFileInput, index: number): RecentFile {
  const key =
    input.target.kind === 'attachment'
      ? attachmentIdentityKey(input.target.id)
      : pathIdentityKey(input.target.projectId, input.target.location);
  return {
    key,
    name: input.name,
    source: {
      conversationKey: `topic-fixture-${index}`,
      messageId: `m-fixture-${index}`,
      sentAt: input.sentAt,
      ...(input.projectId ? { projectId: input.projectId } : {}),
      ...(input.projectName ? { projectName: input.projectName } : {}),
    },
    target: input.target,
  };
}

declare global {
  interface Window {
    chatPaletteFixture: typeof fixture;
  }
}
window.chatPaletteFixture = fixture;
