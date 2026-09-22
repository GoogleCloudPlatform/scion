// @vitest-environment happy-dom
/**
 * Tests for TerminalWorkspaceRoot: data-effective-layout attribute
 * and focus outline suppression in single-pane mode (#1716).
 */
import { afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import type { TerminalWorkspaceRoot } from './terminal-workspace-root.js';
import { TerminalSessionRegistry } from './terminal-sessions.js';

// Mock terminal-pane custom element before importing workspace root
vi.mock('@xterm/xterm', () => ({
  Terminal: class {
    cols = 80;
    rows = 24;
    dispose = vi.fn();
    reset = vi.fn();
    write = vi.fn();
    focus = vi.fn();
    blur = vi.fn();
    refresh = vi.fn();
    parser = { registerOscHandler: vi.fn() };
    loadAddon = vi.fn();
    open = vi.fn();
    onData = vi.fn();
    onBinary = vi.fn();
    attachCustomKeyEventHandler = vi.fn();
  },
}));
vi.mock('@xterm/addon-fit', () => ({
  FitAddon: class {
    fit = vi.fn();
  },
}));
vi.mock('@xterm/addon-web-links', () => ({ WebLinksAddon: class {} }));
vi.mock('@xterm/xterm/css/xterm.css?inline', () => ({ default: '' }));

let WorkspaceRoot: typeof TerminalWorkspaceRoot;

beforeAll(async () => {
  // Ensure custom element is registered
  await import('../components/terminal/terminal-pane.js');
  const mod = await import('./terminal-workspace-root.js');
  WorkspaceRoot = mod.TerminalWorkspaceRoot;
});

function getPaneHost(root: TerminalWorkspaceRoot): HTMLElement {
  return root.element.querySelector('.terminal-pane-host') as HTMLElement;
}

/** Flush queueMicrotask-based refresh. */
async function flush(): Promise<void> {
  await Promise.resolve();
}

describe('data-effective-layout attribute (#1716)', () => {
  let root: TerminalWorkspaceRoot;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              id: '11111111-1111-4111-8111-111111111111',
              name: 'test',
              phase: 'running',
            }),
            { status: 200 }
          )
        )
      )
    );
    vi.stubGlobal(
      'WebSocket',
      class {
        onopen = null;
        onclose = null;
        send = vi.fn();
        close = vi.fn();
        readyState = 0;
      }
    );
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        onopen = null;
        close = vi.fn();
        constructor(public url: string) {
          super();
        }
      }
    );
    root = new WorkspaceRoot();
    document.body.append(root.element);
  });

  afterEach(() => {
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('defaults to single on initial load', () => {
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('single');
  });

  it('updates to two-columns when layout is changed', async () => {
    root.layoutManager.setLayout('two-columns');
    await flush();
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('two-columns');
  });

  it('updates to two-rows when layout is changed', async () => {
    root.layoutManager.setLayout('two-rows');
    await flush();
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('two-rows');
  });

  it('updates to four when layout is changed', async () => {
    root.layoutManager.setLayout('four');
    await flush();
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('four');
  });

  it('reverts to single when switching back from multi-pane', async () => {
    root.layoutManager.setLayout('two-columns');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('two-columns');
    root.layoutManager.setLayout('single');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('single');
  });

  it('overrides to single in narrow viewport regardless of active preset', async () => {
    // Simulate narrow viewport by mocking matchMedia
    const narrowQuery = { matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() };
    vi.spyOn(window, 'matchMedia').mockReturnValue(narrowQuery as unknown as MediaQueryList);

    // Recreate root with the narrow mock in place
    root.element.remove();
    root = new WorkspaceRoot();
    document.body.append(root.element);

    root.layoutManager.setLayout('two-columns');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('single');

    root.layoutManager.setLayout('four');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('single');
  });

  it('overrides to single when a pane is zoomed', async () => {
    const registry = new TerminalSessionRegistry({
      hubUrl: window.location.origin,
      accountId: 'test',
    });

    // Create a session so we can zoom it
    const session = root.create(registry, '11111111-1111-4111-8111-111111111111');
    root.layoutManager.setLayout('two-columns');
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('two-columns');

    root.layoutManager.zoom(session.state.key);
    await flush();
    expect(getPaneHost(root).dataset.effectiveLayout).toBe('single');
  });
});

describe('focus outline suppression in single-pane mode (#1716)', () => {
  let root: TerminalWorkspaceRoot;

  beforeEach(() => {
    vi.stubGlobal('requestAnimationFrame', (cb: FrameRequestCallback) => {
      cb(0);
      return 0;
    });
    vi.stubGlobal(
      'fetch',
      vi.fn(() =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              id: '11111111-1111-4111-8111-111111111111',
              name: 'test',
              phase: 'running',
            }),
            { status: 200 }
          )
        )
      )
    );
    vi.stubGlobal(
      'WebSocket',
      class {
        onopen = null;
        onclose = null;
        send = vi.fn();
        close = vi.fn();
        readyState = 0;
      }
    );
    vi.stubGlobal(
      'EventSource',
      class extends EventTarget {
        onopen = null;
        close = vi.fn();
        constructor(public url: string) {
          super();
        }
      }
    );
    root = new WorkspaceRoot();
    document.body.append(root.element);
  });

  afterEach(() => {
    root.element.remove();
    vi.unstubAllGlobals();
    vi.restoreAllMocks();
  });

  it('CSS focus rule targets only multi-pane layouts via data-effective-layout', () => {
    // Verify the installed stylesheet contains the scoped selector
    const styles = document.querySelectorAll('style');
    let found = false;
    for (const style of styles) {
      const text = style.textContent ?? '';
      if (
        text.includes("data-effective-layout='two-columns']") &&
        text.includes("data-effective-layout='two-rows']") &&
        text.includes("data-effective-layout='four']") &&
        text.includes('scion-terminal-pane[data-focused]')
      ) {
        found = true;
        // Ensure the old unconditional rule is gone
        // The old rule was: scion-terminal-pane[data-focused] { outline: ... }
        // without the parent selector prefix
        const lines = text.split('\n');
        const unconditionalFocusRule = lines.some(
          (line) =>
            line.trim().startsWith('scion-terminal-pane[data-focused]') &&
            !line.includes('data-effective-layout')
        );
        expect(unconditionalFocusRule).toBe(false);
        break;
      }
    }
    expect(found).toBe(true);
  });

  it('data-effective-layout is single when in single-pane mode so CSS focus rule does not match', () => {
    const host = getPaneHost(root);
    // In single-pane mode (default), the CSS selector won't match because
    // data-effective-layout='single' is not in the selector list
    expect(host.dataset.effectiveLayout).toBe('single');
  });

  it('switching from multi-pane to single-pane changes data-effective-layout so focus outline CSS stops applying', async () => {
    root.layoutManager.setLayout('two-columns');
    await flush();
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('two-columns');

    root.layoutManager.setLayout('single');
    await flush();
    expect(host.dataset.effectiveLayout).toBe('single');
    // CSS rule no longer matches because data-effective-layout='single' is not targeted
  });

  it('switching from single-pane to multi-pane restores data-effective-layout so focus outline CSS applies', async () => {
    const host = getPaneHost(root);
    expect(host.dataset.effectiveLayout).toBe('single');

    root.layoutManager.setLayout('two-columns');
    await flush();
    expect(host.dataset.effectiveLayout).toBe('two-columns');
    // CSS rule now matches again for focused panes
  });
});
