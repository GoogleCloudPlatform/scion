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

import { describe, it, expect, vi, afterEach } from 'vitest';
import {
  ChatPanelHistory,
  readPanelEntry,
  withPanelEntry,
  TRAVERSAL_TIMEOUT_MS,
  isBefore,
  newRun,
  type ChatPanel,
} from './chat-panel-history.js';
import { IN_PAGE_STATE_KEY } from '../../client/route-history.js';

/**
 * A session history stack. `go` moves at once, as a traversal would; the
 * test delivers the `popstate` itself, through `popped`.
 */
class FakeHistory {
  entries: Array<{ state: unknown; url: string }>;
  index = 0;
  goCalls: number[] = [];
  pushes = 0;
  replaces = 0;

  constructor(state: unknown = null, url = '/chat/space/topic') {
    this.entries = [{ state, url }];
  }

  get state(): unknown {
    return this.entries[this.index].state;
  }

  get url(): string {
    return this.entries[this.index].url;
  }

  pushState(state: unknown, _unused: string, url?: string | URL | null): void {
    this.pushes++;
    this.entries = this.entries.slice(0, this.index + 1);
    this.entries.push({ state: structuredClone(state), url: url ? String(url) : this.url });
    this.index++;
  }

  replaceState(state: unknown, _unused: string, url?: string | URL | null): void {
    this.replaces++;
    this.entries[this.index] = {
      state: structuredClone(state),
      url: url ? String(url) : this.url,
    };
  }

  go(delta = 0): void {
    this.goCalls.push(delta);
    this.index = Math.max(0, Math.min(this.entries.length - 1, this.index + delta));
  }

  panels(): Array<ChatPanel | undefined> {
    return this.entries.map((e) => readPanelEntry(e.state)?.panel);
  }
}

/** An entry's panel and the panels beneath it, without its run id. */
function rec(state: unknown): { panel: ChatPanel; below: ChatPanel[] } | null {
  const entry = readPanelEntry(state);
  return entry ? { panel: entry.panel, below: entry.below } : null;
}

function setup(start: ChatPanel | null = 'left'): { fake: FakeHistory; ph: ChatPanelHistory } {
  const fake = new FakeHistory(
    start ? withPanelEntry({}, { panel: start, below: [], run: 1 }) : {}
  );
  return { fake, ph: new ChatPanelHistory(fake as unknown as History) };
}

afterEach(() => {
  vi.useRealTimers();
});

describe('panel entries in history state', () => {
  it('reads back what it writes, keeping other state keys', () => {
    const state = withPanelEntry(
      { other: 1 },
      { panel: 'right', below: ['left', 'center'], run: 7 }
    );
    expect(state.other).toBe(1);
    expect(readPanelEntry(state)).toEqual({ panel: 'right', below: ['left', 'center'], run: 7 });
  });

  it('ignores states without a valid record', () => {
    expect(readPanelEntry(null)).toBeNull();
    expect(readPanelEntry({})).toBeNull();
    expect(readPanelEntry({ [IN_PAGE_STATE_KEY]: { panel: 'up' } })).toBeNull();
    expect(rec({ [IN_PAGE_STATE_KEY]: { panel: 'center', below: ['x', 'left'] } })).toEqual({
      panel: 'center',
      below: ['left'],
    });
  });
});

describe('ChatPanelHistory', () => {
  it('pushes an entry on the same URL for each move to a deeper panel', () => {
    const { fake, ph } = setup('left');

    expect(ph.move('left', 'center')).toBe('set');
    expect(ph.move('center', 'right')).toBe('set');

    expect(fake.panels()).toEqual(['left', 'center', 'right']);
    expect(rec(fake.state)).toEqual({ panel: 'right', below: ['left', 'center'] });
    expect(new Set(fake.entries.map((e) => e.url))).toEqual(new Set(['/chat/space/topic']));
  });

  it('adds no entry for a move to the panel already recorded (a tap and a swipe together)', () => {
    const { fake, ph } = setup('left');
    ph.move('left', 'center');
    // The second input of the pair arrives after the first already moved.
    expect(ph.move('center', 'center')).toBe('set');
    expect(ph.move('left', 'center')).toBe('set');
    expect(fake.entries).toHaveLength(2);
  });

  it('goes back through history to an entry showing a shallower panel, and shows it on popstate', () => {
    const { fake, ph } = setup('left');
    ph.move('left', 'center');
    ph.move('center', 'right');
    const pushes = fake.pushes;

    expect(ph.move('right', 'center')).toBe('wait');
    expect(fake.goCalls).toEqual([-1]);
    expect(ph.popped(fake.state, false)).toBe('center');

    expect(ph.move('center', 'left')).toBe('wait');
    expect(fake.goCalls).toEqual([-1, -1]);
    expect(ph.popped(fake.state, false)).toBe('left');

    // Popping wrote nothing: Forward still has both entries to redo.
    expect(fake.pushes).toBe(pushes);
    expect(fake.panels()).toEqual(['left', 'center', 'right']);
    fake.go(1);
    expect(ph.popped(fake.state, false)).toBe('center');
    fake.go(1);
    expect(ph.popped(fake.state, false)).toBe('right');
  });

  it('goes back several entries at once for a jump to the rail', () => {
    const { fake, ph } = setup('left');
    ph.move('left', 'center');
    ph.move('center', 'right');
    expect(ph.move('right', 'left')).toBe('wait');
    expect(fake.goCalls).toEqual([-2]);
    expect(ph.popped(fake.state, false)).toBe('left');
  });

  it('drops moves while its traversal is under way, so two inputs go back only once', () => {
    const { fake, ph } = setup('left');
    ph.move('left', 'center');
    ph.move('center', 'right');

    expect(ph.move('right', 'center')).toBe('wait');
    expect(ph.move('right', 'center')).toBe('ignore');
    expect(ph.move('right', 'left')).toBe('ignore');
    expect(fake.goCalls).toEqual([-1]);

    // Once its popstate has arrived, moves are taken again.
    expect(ph.popped(fake.state, false)).toBe('center');
    expect(ph.move('center', 'left')).toBe('wait');
    expect(fake.goCalls).toEqual([-1, -1]);
  });

  it('accepts moves again if the popstate never comes', () => {
    vi.useFakeTimers();
    const { fake, ph } = setup('left');
    ph.move('left', 'center');
    // A move the browser never acts on: history stays where it is.
    fake.go = (delta = 0): void => {
      fake.goCalls.push(delta);
    };
    expect(ph.move('center', 'left')).toBe('wait');
    expect(ph.move('center', 'left')).toBe('ignore');
    vi.advanceTimersByTime(TRAVERSAL_TIMEOUT_MS);
    expect(ph.move('center', 'left')).toBe('wait');
    expect(fake.goCalls).toEqual([-1, -1]);
  });

  it('replaces the entry when nothing beneath shows the target (a deep link)', () => {
    // A deep link lands on the conversation with no rail entry beneath.
    const { fake, ph } = setup('center');
    expect(ph.move('center', 'left')).toBe('set');
    expect(fake.goCalls).toEqual([]);
    expect(fake.entries).toHaveLength(1);
    expect(rec(fake.state)).toEqual({ panel: 'left', below: [] });

    // From there the rail is beneath the conversation as usual.
    ph.move('left', 'center');
    expect(fake.panels()).toEqual(['left', 'center']);
  });

  it('treats an entry with no record as showing the current panel', () => {
    const { fake, ph } = setup(null);
    expect(ph.move('left', 'center')).toBe('set');
    expect(rec(fake.state)).toEqual({ panel: 'center', below: ['left'] });
    // The entry left behind is recorded too, so Back to it shows the rail.
    expect(fake.panels()).toEqual(['left', 'center']);
  });

  describe('opening a conversation', () => {
    it('moves the rail entry to the new URL, beneath the conversation entry', () => {
      const { fake, ph } = setup('left');
      fake.entries[0].url = '/chat/space/old-topic';

      const state = ph.stateForPush('center', '/chat/space/new-topic');
      // The page pushes the new URL through the router with this state.
      fake.pushState(state, '', '/chat/space/new-topic');

      expect(fake.entries.map((e) => [e.url, rec(e.state)])).toEqual([
        ['/chat/space/new-topic', { panel: 'left', below: [] }],
        ['/chat/space/new-topic', { panel: 'center', below: ['left'] }],
      ]);
    });

    it('starts a new base entry when opened from another panel', () => {
      const { fake, ph } = setup('left');
      ph.move('left', 'center');
      ph.move('center', 'right');
      const replaces = fake.replaces;

      const state = ph.stateForPush('center', '/chat/dm/peer');

      expect(fake.replaces).toBe(replaces);
      expect(rec(state)).toEqual({ panel: 'center', below: [] });
    });

    it('leaves the rail entry alone for a URL the page will rewrite', () => {
      const { fake, ph } = setup('left');
      const state = ph.stateForPush('center', null);
      expect(fake.replaces).toBe(0);
      expect(rec(state)).toEqual({ panel: 'center', below: [] });
    });

    it('records the rail for a push that shows the rail', () => {
      const { fake, ph } = setup('left');
      const state = ph.stateForPush('left', '/chat');
      expect(fake.replaces).toBe(0);
      expect(rec(state)).toEqual({ panel: 'left', below: [] });
    });
  });

  it('a back move landing on an entry replaced since corrects it to the move target', () => {
    // A deep link opens on the conversation; Members; back to the
    // conversation; the back control replaces the base with the rail.
    const { fake, ph } = setup('center');
    ph.move('center', 'right');
    expect(ph.move('right', 'center')).toBe('wait');
    expect(ph.popped(fake.state, false)).toBe('center');
    expect(ph.move('center', 'left')).toBe('set');
    expect(fake.panels()).toEqual(['left', 'right']);
    // Forward to the members entry, then its back control asks for the
    // conversation: the entry beneath now records the rail.
    fake.go(1);
    expect(ph.popped(fake.state, false)).toBe('right');
    expect(ph.move('right', 'center')).toBe('wait');
    expect(ph.popped(fake.state, false)).toBe('center');
    expect(fake.panels()).toEqual(['center', 'right']);
  });

  it('orders entries along history by run, then index in the run', () => {
    const first = newRun();
    const second = newRun();
    expect(second).toBeGreaterThan(first);
    const a = { panel: 'right' as const, below: ['left', 'center'] as ChatPanel[], run: first };
    const b = { panel: 'center' as const, below: [] as ChatPanel[], run: second };
    expect(isBefore(a, b)).toBe(true);
    expect(isBefore(b, a)).toBe(false);
    expect(isBefore({ ...a, below: ['left'] }, a)).toBe(true);
    expect(isBefore(a, a)).toBe(false);
  });

  describe('sync', () => {
    it('records a panel the page changed itself without adding an entry', () => {
      const { fake, ph } = setup('left');
      ph.sync('center');
      expect(fake.entries).toHaveLength(1);
      expect(rec(fake.state)).toEqual({ panel: 'center', below: [] });
    });

    it('keeps what is beneath, and writes nothing when already in step', () => {
      const { fake, ph } = setup('left');
      ph.move('left', 'center');
      const replaces = fake.replaces;
      ph.sync('center');
      expect(fake.replaces).toBe(replaces);
      ph.sync('right');
      expect(rec(fake.state)).toEqual({ panel: 'right', below: ['left'] });
    });

    it('writes nothing while a traversal is under way', () => {
      const { fake, ph } = setup('left');
      ph.move('left', 'center');
      ph.move('center', 'left');
      const replaces = fake.replaces;
      ph.sync('center');
      expect(fake.replaces).toBe(replaces);
    });
  });

  describe('in the wide layout (all panels on screen)', () => {
    /** A phone-made stack: rail, conversation, members, then the page is wide. */
    function wideStack(): { fake: FakeHistory; ph: ChatPanelHistory } {
      const fake = new FakeHistory({}, '/before');
      fake.pushState(
        withPanelEntry({}, { panel: 'left', below: [], run: 1 }),
        '',
        '/chat/space/topic'
      );
      const ph = new ChatPanelHistory(fake as unknown as History);
      ph.move('left', 'center');
      ph.move('center', 'right');
      fake.goCalls = [];
      return { fake, ph };
    }

    it('Back onto a panel entry goes on past the first entry of the URL', () => {
      const { fake, ph } = wideStack();
      fake.go(-1); // the user's Back
      expect(ph.popped(fake.state, true)).toBeNull();
      // From the conversation entry, two more: past the rail entry, off the URL.
      expect(fake.goCalls).toEqual([-1, -2]);
      expect(fake.url).toBe('/before');
    });

    it('Back onto the first entry of the URL goes on to the page before', () => {
      const { fake, ph } = wideStack();
      fake.go(-2); // a long-press Back straight to the rail entry
      expect(ph.popped(fake.state, true)).toBeNull();
      expect(fake.goCalls).toEqual([-2, -1]);
      expect(fake.url).toBe('/before');
    });

    it('Forward onto a panel entry is left alone: no traversal, never a loop', () => {
      const { fake, ph } = wideStack();
      fake.pushState({}, '', '/after');
      fake.index = 2; // the rail entry, as after Back from /after
      ph.observe();
      fake.go(1); // the user's Forward, onto the conversation entry
      expect(ph.popped(fake.state, true)).toBeNull();
      expect(fake.goCalls).toEqual([1]);
    });

    it('a second run on the same URL ahead of a phone run: one Back leaves, with one traversal', () => {
      // On a phone: rail, then the conversation; then the open thread is
      // picked again from the conversation, which starts a new run.
      const { fake, ph } = setup('left');
      ph.move('left', 'center');
      fake.pushState(ph.stateForPush('center', null), '', '/chat/space/topic');
      fake.goCalls = [];

      fake.go(-1); // wide, the user's Back: onto the earlier run's conversation entry
      expect(ph.popped(fake.state, true)).toBeNull();
      expect(fake.goCalls).toEqual([-1, -2]);
      // The traversal's own arrival never starts another, whatever it lands on.
      expect(ph.popped(fake.state, true)).toBeNull();
      expect(ph.popped(fake.state, true)).toBeNull();
      expect(fake.goCalls).toEqual([-1, -2]);
    });

    it("never chains on from its own traversal's arrival, even onto an earlier entry", () => {
      const { fake, ph } = wideStack();
      fake.go(-1);
      ph.popped(fake.state, true); // starts go(-2)
      // Its arrival lands on an entry history orders earlier still.
      const arrival = withPanelEntry({}, { panel: 'center', below: ['left'], run: 0 });
      expect(ph.popped(arrival, true)).toBeNull();
      expect(fake.goCalls).toEqual([-1, -2]);
    });

    it('does nothing without knowing the direction (coming from an entry with no record)', () => {
      const { fake, ph } = wideStack();
      fake.pushState({}, '', '/chat/space/topic'); // an unrecorded entry on the same URL
      ph.observe();
      fake.goCalls = [];
      fake.go(-1); // onto the members entry
      expect(ph.popped(fake.state, true)).toBeNull();
      expect(fake.goCalls).toEqual([-1]);
      // The next Back is known to be Back: it leaves.
      fake.go(-1);
      expect(ph.popped(fake.state, true)).toBeNull();
      expect(fake.goCalls).toEqual([-1, -1, -2]);
      expect(fake.url).toBe('/before');
    });

    it('writes nothing, so the entries still hold back on a phone', () => {
      const { fake, ph } = wideStack();
      const before = JSON.stringify(fake.entries);
      fake.go(-1);
      ph.popped(fake.state, true);
      expect(JSON.stringify(fake.entries)).toBe(before);
    });

    it('a page created wide on a panel entry goes back to the first entry of the URL', () => {
      const { fake } = wideStack();
      const fresh = new ChatPanelHistory(fake as unknown as History);
      fresh.observe();
      fresh.toBase();
      expect(fake.goCalls).toEqual([-2]);
      expect(fake.url).toBe('/chat/space/topic');
      // Arriving there is not a step to skip on from.
      expect(fresh.popped(fake.state, true)).toBeNull();
      expect(fake.goCalls).toEqual([-2]);
      // From there Back leaves the URL.
      fake.go(-1);
      expect(fresh.popped(fake.state, true)).toBeNull();
      expect(fake.url).toBe('/before');
    });

    it('leaves an entry with nothing beneath alone when coming from one too', () => {
      const { fake, ph } = setup('left');
      ph.observe();
      expect(ph.popped(fake.state, true)).toBeNull();
      expect(fake.goCalls).toEqual([]);
    });
  });
});
