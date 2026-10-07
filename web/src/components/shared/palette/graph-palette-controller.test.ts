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

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { LitElement, html } from 'lit';
import { GraphPaletteController, buildGraphAgentCandidates } from './graph-palette-controller.js';
import { QuickPaletteHost } from './quick-palette-host.js';
import type { ScionQuickPalette } from './quick-palette.js';
import type { ScionAgentTreeView } from '../agent-tree-view.js';
import type { Agent } from '../../../shared/types.js';
import {
  GRAPH_PALETTE_AVAILABILITY_EVENT,
  GRAPH_PALETTE_OPEN_REQUEST_EVENT,
  isGraphPaletteAvailable,
} from '../../../client/graph-palette-events.js';

function agent(id: string, name: string, project = 'Project'): Agent {
  return { id, name, slug: id, project, projectId: 'p1', phase: 'running' } as Agent;
}

const revealAgent = vi.fn<(id: string) => boolean>();
const focusAgentNode = vi.fn<(id: string) => boolean>();

/**
 * Stands in for `<scion-agent-tree-view>`: the agents it shows,
 * `revealAgent` and `focusAgentNode`.
 */
class TestTreeView extends HTMLElement {
  agents: Agent[] = [];
  revealAgent(id: string): boolean {
    return revealAgent(id);
  }
  focusAgentNode(id: string): boolean {
    return focusAgentNode(id);
  }
}
customElements.define('test-tree-view', TestTreeView);

/**
 * A minimal graph page. `active` stands in for its view mode: the tree view
 * is rendered only while it is set, as on a real page. `loading` replaces
 * the whole template, as a page's loading state does, and `framed` renders
 * the tree view through a different root template. `treeHidden` puts the
 * tree view in a hidden subtree of the page's own render root.
 */
class TestGraphPage extends LitElement {
  static override properties = {
    active: { type: Boolean },
    loading: { type: Boolean },
    framed: { type: Boolean },
    treeHidden: { type: Boolean },
    agents: { attribute: false },
  };
  active = true;
  loading = false;
  framed = false;
  treeHidden = false;
  agents: Agent[] = [];
  readonly palette = new GraphPaletteController(this, {
    treeView: (): ScionAgentTreeView | null =>
      this.renderRoot.querySelector('test-tree-view') as unknown as ScionAgentTreeView | null,
  });

  override render(): unknown {
    if (this.loading) return html`<p>loading</p>`;
    if (!this.active) return html`<p>list</p>`;
    const tree = html`<div ?hidden=${this.treeHidden}>
      <test-tree-view .agents=${this.agents}></test-tree-view>
    </div>`;
    return this.framed ? html`<section>${tree}</section>` : tree;
  }
}
customElements.define('test-graph-page', TestGraphPage);

let page: TestGraphPage;

function pressShortcut(init: KeyboardEventInit = { metaKey: true }): KeyboardEvent {
  const e = new KeyboardEvent('keydown', {
    key: 'k',
    bubbles: true,
    composed: true,
    cancelable: true,
    ...init,
  });
  document.body.dispatchEvent(e);
  return e;
}

/** Fires `type` from the palette's own dialog, composed, as Shoelace does. */
function fireFromDialog(palette: ScionQuickPalette, type: 'sl-hide' | 'sl-after-hide'): void {
  palette
    .shadowRoot!.querySelector('sl-dialog')!
    .dispatchEvent(new Event(type, { bubbles: true, composed: true }));
}

function paletteEl(): ScionQuickPalette | null {
  return document.querySelector('scion-quick-palette');
}

function candidateIds(palette: ScionQuickPalette): (string | false)[] {
  return (palette.groups.agents?.candidates ?? []).map(
    (c) => c.target.kind === 'agent' && c.target.agentId
  );
}

function selectAgent(palette: ScionQuickPalette, agentId: string): void {
  palette.dispatchEvent(
    new CustomEvent('palette-select', {
      detail: { target: { kind: 'agent', agentId, displayName: agentId } },
    })
  );
}

function openRequest(): void {
  document.body.dispatchEvent(
    new CustomEvent(GRAPH_PALETTE_OPEN_REQUEST_EVENT, { bubbles: true, composed: true })
  );
}

async function waitForOpenPalette(): Promise<ScionQuickPalette> {
  const palette = await vi.waitFor(
    () => {
      const el = paletteEl();
      if (!el?.open) throw new Error('palette not open yet');
      return el;
    },
    { timeout: 5000 }
  );
  await vi.waitFor(() => expect(palette.groups.agents?.status).toBe('ready'));
  return palette;
}

function nextTask(): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, 0));
}

beforeEach(async () => {
  revealAgent.mockReset().mockReturnValue(true);
  focusAgentNode.mockReset().mockReturnValue(true);
  page = document.createElement('test-graph-page') as TestGraphPage;
  page.agents = [agent('a1', 'Alpha'), agent('a2', 'Beta')];
  document.body.append(page);
  await page.updateComplete;
});

afterEach(() => {
  page.remove();
  document.body.replaceChildren();
  vi.restoreAllMocks();
});

describe('buildGraphAgentCandidates', () => {
  it('builds one agent row per agent, in order', () => {
    const rows = buildGraphAgentCandidates([agent('b', 'Bravo', 'P2'), agent('a', 'Alpha')]);
    expect(rows.map((r) => r.target)).toEqual([
      { kind: 'agent', agentId: 'b', displayName: 'Bravo' },
      { kind: 'agent', agentId: 'a', displayName: 'Alpha' },
    ]);
    expect(rows[0].group).toBe('agents');
    expect(rows[0].secondaryLabel).toBe('P2');
  });

  it('skips an agent without an id', () => {
    const rows = buildGraphAgentCandidates([agent('', 'Nameless'), agent('a', 'Alpha')]);
    expect(rows.map((r) => r.id)).toEqual([JSON.stringify(['agent', 'a'])]);
  });
});

describe('GraphPaletteController', () => {
  it('opens on Cmd+K and Ctrl+K with the page agents as candidates, fetching nothing', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const e = pressShortcut({ metaKey: true });
    expect(e.defaultPrevented).toBe(true);
    const palette = await waitForOpenPalette();

    expect(palette.label).toBe('Jump to agent');
    expect(palette.groups.agents?.candidates.map((c) => c.target)).toEqual([
      { kind: 'agent', agentId: 'a1', displayName: 'Alpha' },
      { kind: 'agent', agentId: 'a2', displayName: 'Beta' },
    ]);
    expect(fetchSpy).not.toHaveBeenCalled();

    const close = pressShortcut({ metaKey: true });
    expect(palette.open).toBe(false);
    expect(close.defaultPrevented).toBe(true);

    pressShortcut({ ctrlKey: true });
    await waitForOpenPalette();
  });

  it('on a Mac, leaves Ctrl+K typed in a text field to the field', async () => {
    vi.spyOn(navigator, 'platform', 'get').mockReturnValue('MacIntel');
    const input = document.createElement('input');
    page.renderRoot.append(input);
    const pressIn = (init: KeyboardEventInit): KeyboardEvent => {
      const e = new KeyboardEvent('keydown', {
        key: 'k',
        bubbles: true,
        composed: true,
        cancelable: true,
        ...init,
      });
      input.dispatchEvent(e);
      return e;
    };

    expect(pressIn({ ctrlKey: true }).defaultPrevented).toBe(false);
    await nextTask();
    expect(page.palette.isOpen).toBe(false);
    expect(paletteEl()).toBeNull();

    expect(pressIn({ metaKey: true }).defaultPrevented).toBe(true);
    await waitForOpenPalette();
  });

  it('reads the agents again on every open', async () => {
    page.palette.open();
    let palette = await waitForOpenPalette();
    pressShortcut();
    expect(palette.open).toBe(false);

    page.agents = [agent('a2', 'Beta')];
    await page.updateComplete;
    pressShortcut();
    palette = await waitForOpenPalette();
    expect(candidateIds(palette)).toEqual(['a2']);
  });

  it('keeps the candidates in step with the tree view while open', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();

    page.agents = [agent('a2', 'Beta'), agent('a3', 'Gamma')];
    await page.updateComplete;
    expect(palette.groups.agents?.status).toBe('ready');
    expect(candidateIds(palette)).toEqual(['a2', 'a3']);

    selectAgent(palette, 'a1');
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    expect(revealAgent).not.toHaveBeenCalled();
    expect(focusAgentNode).not.toHaveBeenCalled();
  });

  it('does not rebuild the candidates for a new array of the same agents', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();
    const setCandidates = vi.spyOn(QuickPaletteHost.prototype, 'setCandidates');

    page.agents = [...page.agents];
    await page.updateComplete;
    expect(setCandidates).not.toHaveBeenCalled();

    page.agents = [page.agents[0], { ...page.agents[1], name: 'Beta 2' }];
    await page.updateComplete;
    expect(setCandidates).toHaveBeenCalledTimes(1);
    expect(palette.groups.agents?.candidates.map((c) => c.label)).toEqual(['Alpha', 'Beta 2']);
  });

  it('drops agents removed from the end of the list while open', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();
    expect(candidateIds(palette)).toEqual(['a1', 'a2']);

    page.agents = [page.agents[0]];
    await page.updateComplete;
    expect(candidateIds(palette)).toEqual(['a1']);

    selectAgent(palette, 'a2');
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    expect(revealAgent).not.toHaveBeenCalled();
  });

  it('opens from the header open request', async () => {
    openRequest();
    await waitForOpenPalette();
  });

  it('ignores the header open request while another modal is open', async () => {
    const other = document.createElement('sl-dialog') as HTMLElement & { open: boolean };
    other.open = true;
    document.body.append(other);

    openRequest();
    await nextTask();

    expect(page.palette.isOpen).toBe(false);
    expect(paletteEl()).toBeNull();
  });

  it('is unavailable while the tree view is not rendered or shows no agents', async () => {
    page.loading = true;
    await page.updateComplete;
    expect(page.palette.isAvailable).toBe(false);

    page.loading = false;
    page.agents = [];
    await page.updateComplete;
    expect(page.palette.isAvailable).toBe(false);
    pressShortcut();
    openRequest();
    await nextTask();
    expect(page.palette.isOpen).toBe(false);

    page.agents = [agent('a1', 'Alpha')];
    await page.updateComplete;
    expect(page.palette.isAvailable).toBe(true);
  });

  it('is unavailable while the tree view is in a hidden subtree of the page', async () => {
    page.treeHidden = true;
    await page.updateComplete;
    expect(page.palette.isAvailable).toBe(false);

    page.treeHidden = false;
    await page.updateComplete;
    expect(page.palette.isAvailable).toBe(true);
  });

  it('stays open while the page swaps its root template around the tree view', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();

    page.framed = true;
    await page.updateComplete;

    expect(palette.isConnected).toBe(true);
    expect(palette.open).toBe(true);
    expect(page.palette.isOpen).toBe(true);
  });

  it('survives the page replacing its whole template', async () => {
    pressShortcut();
    let palette = await waitForOpenPalette();
    pressShortcut();
    expect(palette.open).toBe(false);

    page.loading = true;
    await page.updateComplete;
    page.loading = false;
    await page.updateComplete;

    pressShortcut();
    palette = await waitForOpenPalette();
    expect(palette.isConnected).toBe(true);
    expect(page.palette.isOpen).toBe(true);
  });

  it('is unavailable, and ignores the shortcut and open request, while the graph is not showing', async () => {
    page.active = false;
    await page.updateComplete;

    expect(page.palette.isAvailable).toBe(false);
    expect(isGraphPaletteAvailable()).toBe(false);
    const e = pressShortcut();
    openRequest();
    await nextTask();
    expect(e.defaultPrevented).toBe(false);
    expect(page.palette.isOpen).toBe(false);
    expect(paletteEl()).toBeNull();
  });

  it('announces availability changes on window', async () => {
    expect(isGraphPaletteAvailable()).toBe(true);
    const onChange = vi.fn();
    window.addEventListener(GRAPH_PALETTE_AVAILABILITY_EVENT, onChange);
    try {
      page.active = false;
      await page.updateComplete;
      expect(onChange).toHaveBeenCalledTimes(1);
      expect(isGraphPaletteAvailable()).toBe(false);

      page.active = true;
      await page.updateComplete;
      expect(onChange).toHaveBeenCalledTimes(2);
      expect(isGraphPaletteAvailable()).toBe(true);
    } finally {
      window.removeEventListener(GRAPH_PALETTE_AVAILABILITY_EVENT, onChange);
    }
  });

  it('closes when the graph stops showing', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();

    page.active = false;
    await page.updateComplete;

    expect(palette.open).toBe(false);
    expect(page.palette.isOpen).toBe(false);
  });

  it('closes and becomes unavailable while an ancestor is hidden', async () => {
    const outlet = document.createElement('div');
    document.body.append(outlet);
    outlet.append(page);
    await page.updateComplete;
    pressShortcut();
    const palette = await waitForOpenPalette();

    outlet.hidden = true;
    await vi.waitFor(() => expect(page.palette.isAvailable).toBe(false));
    expect(palette.open).toBe(false);
    pressShortcut();
    await nextTask();
    expect(page.palette.isOpen).toBe(false);

    outlet.hidden = false;
    await vi.waitFor(() => expect(page.palette.isAvailable).toBe(true));
  });

  it('does not open over another open modal', async () => {
    const other = document.createElement('sl-dialog') as HTMLElement & { open: boolean };
    other.open = true;
    document.body.append(other);

    const e = pressShortcut();
    await nextTask();

    expect(e.defaultPrevented).toBe(false);
    expect(page.palette.isOpen).toBe(false);
  });

  it('reveals the picked agent at once, and focuses it once the close settles', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();

    selectAgent(palette, 'a2');
    expect(palette.open).toBe(false);
    expect(revealAgent).toHaveBeenCalledTimes(1);
    expect(revealAgent).toHaveBeenCalledWith('a2');
    expect(focusAgentNode).not.toHaveBeenCalled();

    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(focusAgentNode).toHaveBeenCalledTimes(1);
    expect(focusAgentNode).toHaveBeenCalledWith('a2');
  });

  it('moves no focus for a pick the tree view could not reveal', async () => {
    revealAgent.mockReturnValue(false);
    pressShortcut();
    const palette = await waitForOpenPalette();

    selectAgent(palette, 'a2');
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(revealAgent).toHaveBeenCalledWith('a2');
    expect(focusAgentNode).not.toHaveBeenCalled();
  });

  it('keeps the reveal but moves no focus once the palette has reopened', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();

    selectAgent(palette, 'a2');
    fireFromDialog(palette, 'sl-after-hide');
    page.palette.open();
    await nextTask();

    expect(revealAgent).toHaveBeenCalledWith('a2');
    expect(focusAgentNode).not.toHaveBeenCalled();
    expect(page.palette.isOpen).toBe(true);
  });

  it('a shortcut during the close animation reopens it once the animation ends, keeping the reveal of the pick', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();

    selectAgent(palette, 'a2');
    fireFromDialog(palette, 'sl-hide');
    pressShortcut();
    await nextTask();
    expect(page.palette.isOpen).toBe(true);
    expect(palette.open).toBe(false);
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();
    await nextTask();

    expect(palette.open).toBe(true);
    expect(revealAgent).toHaveBeenCalledWith('a2');
    expect(focusAgentNode).not.toHaveBeenCalled();
  });

  it('a dismiss reveals and focuses nothing in the tree view', async () => {
    pressShortcut();
    const palette = await waitForOpenPalette();

    palette.dispatchEvent(new CustomEvent('palette-dismiss', { detail: { reason: 'escape' } }));
    fireFromDialog(palette, 'sl-after-hide');
    await nextTask();

    expect(revealAgent).not.toHaveBeenCalled();
    expect(focusAgentNode).not.toHaveBeenCalled();
  });

  it('on disconnect removes the palette, becomes unavailable and stops listening', async () => {
    pressShortcut();
    await waitForOpenPalette();
    const removed = vi.spyOn(document, 'removeEventListener');
    const observerDisconnect = vi.spyOn(MutationObserver.prototype, 'disconnect');

    page.remove();

    expect(removed.mock.calls.map(([type]) => type).sort()).toEqual(
      ['keydown', GRAPH_PALETTE_OPEN_REQUEST_EVENT].sort()
    );
    expect(observerDisconnect).toHaveBeenCalledTimes(1);

    expect(page.palette.isAvailable).toBe(false);
    expect(isGraphPaletteAvailable()).toBe(false);
    expect(paletteEl()).toBeNull();
    const e = pressShortcut();
    expect(e.defaultPrevented).toBe(false);
  });

  it('works again after reconnecting', async () => {
    page.remove();
    document.body.append(page);
    await page.updateComplete;

    expect(page.palette.isAvailable).toBe(true);
    pressShortcut();
    await waitForOpenPalette();
  });
});
