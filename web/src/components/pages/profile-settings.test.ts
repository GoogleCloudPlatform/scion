import { describe, it, expect, vi, beforeAll, afterEach } from 'vitest';
import { getPreferredTimeZone, setPreferredTimeZone } from '../../utils/time.js';
import { DECISIONS_FLAG, EXEC_CONTAINERS_FLAG, setFeatureFlag } from '../../utils/feature-flags.js';
import {
  preselectRecommendedChoices,
  setPreselectRecommendedChoices,
} from '../../client/decision-preferences.js';

// ── Fetch mock ──

interface ServerConfigFixture {
  status?: number;
  body?: Record<string, unknown>;
}

interface PatchResult {
  status: number;
  body: Record<string, unknown>;
}

interface AuthMeFixture {
  status?: number;
  body?: Record<string, unknown>;
}

// An admin server-config response that still carries runtime-profile zones.
// The page must never request it.
function makeServerConfig(): Record<string, unknown> {
  return {
    schema_version: '1',
    active_profile: 'local',
    profiles: {
      local: { runtime: 'docker', timezone: 'UTC', image_registry: 'reg.example' },
      remote: { runtime: 'kubernetes', timezone: 'Europe/Berlin' },
    },
  };
}

function makeAuthMe(timezone = ''): Record<string, unknown> {
  return {
    id: 'u1',
    email: 'u1@example.com',
    displayName: 'User One',
    preferences: timezone ? { timezone } : {},
  };
}

function createFetchHandler(
  config: ServerConfigFixture,
  onPatch?: (body: Record<string, unknown>) => PatchResult,
  authMe: AuthMeFixture = {},
  onUserPatch?: (body: Record<string, unknown>) => PatchResult
) {
  return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
    const json = (status: number, body: unknown): Promise<Response> =>
      Promise.resolve(
        new Response(JSON.stringify(body), {
          status,
          headers: { 'Content-Type': 'application/json' },
        })
      );

    if (path.endsWith('/api/v1/admin/server-config')) {
      if (init?.method === 'PATCH') {
        const result = onPatch
          ? onPatch(JSON.parse(init.body as string) as Record<string, unknown>)
          : { status: 200, body: { status: 'saved' } };
        return json(result.status, result.body);
      }
      return json(config.status ?? 200, config.body ?? makeServerConfig());
    }
    if (path.endsWith('/auth/me')) {
      return json(authMe.status ?? 200, authMe.body ?? makeAuthMe());
    }
    if (/\/api\/v1\/users\/[^/]+$/.test(path) && init?.method === 'PATCH') {
      const result = onUserPatch
        ? onUserPatch(JSON.parse(init.body as string) as Record<string, unknown>)
        : { status: 200, body: { id: 'u1' } };
      return json(result.status, result.body);
    }
    return json(200, {});
  };
}

// ── Helpers ──

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyEl = any;

async function createComponent(
  fetchHandler: (url: string | URL | Request, init?: RequestInit) => Promise<Response>
): Promise<AnyEl> {
  vi.stubGlobal('fetch', vi.fn(fetchHandler));
  const el = document.createElement('scion-page-profile-settings') as AnyEl;
  document.body.appendChild(el);
  await el.updateComplete;
  // Let the async server-config load settle.
  await new Promise((resolve) => setTimeout(resolve, 50));
  await el.updateComplete;
  return el;
}

/**
 * Waits for a fire-and-forget async handler (e.g. a Lit event listener that
 * calls `void this._handleZoneChange(e)` without awaiting it) to finish and
 * for the resulting state change to render.
 */
async function settle(el: AnyEl): Promise<void> {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await el.updateComplete;
}

function shadowText(el: HTMLElement): string {
  return el.shadowRoot?.textContent ?? '';
}

function patchCalls(): Array<[unknown, RequestInit]> {
  const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
  return (fetchMock.mock.calls as Array<[unknown, RequestInit | undefined]>).filter(
    ([, init]) => init?.method === 'PATCH'
  ) as Array<[unknown, RequestInit]>;
}

// ── Tests ──

describe('scion-page-profile-settings — no agent timezone section', () => {
  let element: AnyEl = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({})));
    await import('./profile-settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    vi.restoreAllMocks();
  });

  function requestedPaths(): string[] {
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
    return (fetchMock.mock.calls as Array<[unknown]>).map(([url]) =>
      typeof url === 'string' ? url : url instanceof URL ? url.pathname : (url as Request).url
    );
  }

  // The runtime-profile timezone was removed from the hub, so the page must
  // neither show its old section nor read admin server config, even when
  // the endpoint is readable and still returns a profile with a zone.
  it('makes no admin server-config request and renders no Agent timezone section', async () => {
    element = await createComponent(createFetchHandler({}));

    expect(requestedPaths().length).toBeGreaterThan(0);
    expect(requestedPaths().filter((p) => p.includes('/api/v1/admin/server-config'))).toEqual([]);
    expect(patchCalls().filter(([url]) => String(url).includes('/admin/'))).toEqual([]);
    expect(shadowText(element)).not.toContain('Agent timezone');
    expect(element.shadowRoot.querySelector('.timezone-row')).toBeNull();
    expect(shadowText(element)).toContain('Display timezone');
  });
});

describe('scion-page-profile-settings — display timezone', () => {
  let element: AnyEl = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({})));
    await import('./profile-settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    setPreferredTimeZone('');
    vi.restoreAllMocks();
  });

  function userPatchCalls(): Array<[unknown, RequestInit]> {
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
    return (fetchMock.mock.calls as Array<[unknown, RequestInit | undefined]>).filter(
      ([url, init]) => init?.method === 'PATCH' && /\/api\/v1\/users\/[^/]+$/.test(String(url))
    ) as Array<[unknown, RequestInit]>;
  }

  /**
   * Drives task 12's real picker UI — focus, type, click the matching
   * dropdown option — instead of dispatching a synthetic `timezone-change`
   * event on the element. Exercises the picker's own `selectZone()`/
   * `emitChange()`, so a mismatch between what the picker actually emits
   * and what this page listens for would fail these tests (review R2-2:
   * a hand-built `CustomEvent` can't catch that, because it's the page's
   * own assumption about the contract, not the picker's real behaviour).
   * Never modifies the picker itself — task 12 owns that file.
   */
  async function selectViaPicker(picker: AnyEl, displayText: string): Promise<void> {
    const input = picker.shadowRoot.querySelector('sl-input');
    input.dispatchEvent(new Event('sl-focus'));
    await picker.updateComplete;
    input.value = displayText;
    input.dispatchEvent(new Event('sl-input'));
    await picker.updateComplete;
    const options = [...picker.shadowRoot.querySelectorAll('.timezone-search-option')] as AnyEl[];
    const option = options.find((o) => o.textContent.trim() === displayText);
    if (!option) {
      throw new Error(`selectViaPicker: no option rendered for "${displayText}"`);
    }
    option.dispatchEvent(new MouseEvent('mousedown', { bubbles: true, cancelable: true }));
  }

  /**
   * Types `text` into the picker and commits it with Enter, without
   * selecting a rendered dropdown option — the path for a value that may
   * not be a recognized zone (the picker itself never validates typed
   * input; callers validate with `isValidTimeZone`), so it has no matching
   * option for `selectViaPicker` to click.
   */
  async function commitTypedViaPicker(picker: AnyEl, text: string): Promise<void> {
    const input = picker.shadowRoot.querySelector('sl-input');
    input.dispatchEvent(new Event('sl-focus'));
    await picker.updateComplete;
    input.value = text;
    input.dispatchEvent(new Event('sl-input'));
    await picker.updateComplete;
    input.dispatchEvent(
      new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true })
    );
  }

  /**
   * The text currently displayed in the picker's own search input.
   * Re-queries the element (the R2-2 fix remounts the picker via `keyed()`
   * on revert, so the old reference is stale) and awaits its own
   * `updateComplete`, since a freshly mounted custom element's first
   * render is a separate microtask from the parent's. Reads the
   * `value` *attribute*, not a `.value` property: task 12's picker
   * template binds its inner `sl-input`'s value with `value=${...}`
   * (an attribute binding), not `.value=${...}`, and `sl-input` is never
   * upgraded to the real Shoelace element in this test environment, so no
   * `.value` property exists to reflect it.
   */
  async function pickerDisplayText(element: AnyEl): Promise<string> {
    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    await picker.updateComplete;
    return picker.shadowRoot.querySelector('sl-input').getAttribute('value');
  }

  it('is visible to every signed-in user, including one who cannot read server config', async () => {
    element = await createComponent(
      createFetchHandler({ status: 403 }, undefined, { body: makeAuthMe() })
    );
    expect(shadowText(element)).toContain('Display timezone');
    expect(shadowText(element)).not.toContain('Agent timezone');
  });

  it('explains that only a pinned time zone is shared with agents and Auto is display-only', async () => {
    element = await createComponent(createFetchHandler({}, undefined, { body: makeAuthMe() }));
    const text = shadowText(element).replace(/\s+/g, ' ');
    expect(text).toContain('Only a pinned time zone is shared with agents you message');
    expect(text).toContain('Auto is used for display only');
  });

  it('loads the current preference (Auto when unset)', async () => {
    element = await createComponent(createFetchHandler({}, undefined, { body: makeAuthMe() }));
    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    expect(picker.value).toBe('');
    expect(picker.getAttribute('empty-label')).toBe('Auto');
  });

  it('loads a configured preference', async () => {
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe('Asia/Tokyo') })
    );
    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    expect(picker.value).toBe('Asia/Tokyo');
  });

  // Review R1-2/R2-2: the picker's event contract is `timezone-change` /
  // `{ timezone }` (task 12's), not the `zone-change` / `{ value }` this
  // page used before the task-11-onto-task-12 rebase. This test pins the
  // binding by driving the picker's *real* UI (focus, type, click the
  // rendered option) so it emits its own genuine event — not a hand-built
  // `CustomEvent('timezone-change', ...)` that only proves the page's own
  // assumption about the contract, which is exactly what round 2 found
  // wasn't enough (R2-2): a mismatch on the picker's side wouldn't fail a
  // test built from a synthetic event.
  it("is wired to the picker's real timezone-change event with e.detail.timezone (review R1-2/R2-2)", async () => {
    let captured: Record<string, unknown> | null = null;
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe() }, (body) => {
        captured = body;
        return { status: 200, body: { id: 'u1' } };
      })
    );

    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    await selectViaPicker(picker, 'Asia/Kathmandu');
    await settle(element);

    expect(captured).toEqual({ preferences: { timezone: 'Asia/Kathmandu' } });
  });

  it('saves a selection with a per-key preferences merge and updates the store live', async () => {
    let captured: Record<string, unknown> | null = null;
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe() }, (body) => {
        captured = body;
        return { status: 200, body: { id: 'u1' } };
      })
    );

    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    await selectViaPicker(picker, 'Asia/Kathmandu');
    await settle(element);

    expect(captured).toEqual({ preferences: { timezone: 'Asia/Kathmandu' } });
    expect(getPreferredTimeZone()).toBe('Asia/Kathmandu');
    expect(shadowText(element)).toContain('Display timezone updated.');
  });

  it('clearing to Auto sends an explicit empty string and updates the store with no reload', async () => {
    let captured: Record<string, unknown> | null = null;
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe('Asia/Tokyo') }, (body) => {
        captured = body;
        return { status: 200, body: { id: 'u1' } };
      })
    );

    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    await selectViaPicker(picker, 'Auto');
    await settle(element);

    expect(captured).toEqual({ preferences: { timezone: '' } });
    expect(getPreferredTimeZone()).toBe('');
  });

  // Review R1-5/R2-2: a failed PATCH must not leave the picker displaying
  // the rejected zone next to the error banner. R2-2 found the original
  // fix (`picker.value = this._displayTimezone`) was a no-op against task
  // 12's picker, and the original test couldn't catch that because it
  // dispatched a synthetic event instead of driving the picker's own typed
  // text — so this asserts on the picker's actual displayed text
  // (its inner `sl-input`'s `value`) after a *real* selection, through a
  // freshly re-queried element (the fix remounts the picker via `keyed()`).
  it('reverts the picker display on a failed PATCH and does not update the store', async () => {
    element = await createComponent(
      createFetchHandler({}, undefined, { body: makeAuthMe() }, () => ({
        status: 400,
        body: { error: { message: 'invalid timezone' } },
      }))
    );

    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    await commitTypedViaPicker(picker, 'Not/AZone');
    await settle(element);

    expect(shadowText(element)).toContain('invalid timezone');
    expect(getPreferredTimeZone()).toBe('');
    expect(await pickerDisplayText(element)).toBe('Auto');
  });

  it('reverts the picker display when the user id has not loaded yet, and does not PATCH', async () => {
    element = await createComponent(createFetchHandler({}, undefined, { status: 500, body: {} }));

    const picker = element.shadowRoot.querySelector('scion-timezone-picker');
    await selectViaPicker(picker, 'Asia/Tokyo');
    await settle(element);

    expect(userPatchCalls()).toHaveLength(0);
    expect(await pickerDisplayText(element)).toBe('Auto');
  });
});

describe('scion-page-profile-settings — alert toggles', () => {
  let element: AnyEl = null;

  class FakeNotification {
    static permission: NotificationPermission = 'granted';
    static requestPermission = vi.fn(() => Promise.resolve(FakeNotification.permission));
  }

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({})));
    await import('./profile-settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    localStorage.clear();
    vi.restoreAllMocks();
  });

  function toggle(el: AnyEl, category: 'chat' | 'agent'): AnyEl {
    return el.shadowRoot?.querySelector(`sl-switch[data-push-category="${category}"]`);
  }

  it('shows separate chat message and agent event toggles', async () => {
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    element = await createComponent(createFetchHandler({}));
    expect(toggle(element, 'chat')?.getAttribute('aria-label')).toBe('Chat message alerts');
    expect(toggle(element, 'agent')?.getAttribute('aria-label')).toBe('Agent event alerts');
    expect(shadowText(element)).toContain('Chat message alerts');
    expect(shadowText(element)).toContain('Agent event alerts');
    expect(shadowText(element)).not.toContain('Enable Push Notifications');
  });

  it('carries an old single opt-in over to both toggles', async () => {
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem('scion-push-notifications', 'true');
    element = await createComponent(createFetchHandler({}));
    expect(toggle(element, 'chat')?.hasAttribute('checked')).toBe(true);
    expect(toggle(element, 'agent')?.hasAttribute('checked')).toBe(true);
  });

  it('turns one category off without touching the other', async () => {
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem('scion-push-chat-messages', 'true');
    localStorage.setItem('scion-push-agent-events', 'true');
    element = await createComponent(createFetchHandler({}));

    const chat = toggle(element, 'chat');
    chat.checked = false;
    chat.dispatchEvent(new CustomEvent('sl-change'));
    await settle(element);

    expect(localStorage.getItem('scion-push-chat-messages')).toBe('false');
    expect(localStorage.getItem('scion-push-agent-events')).toBe('true');
    expect(toggle(element, 'chat')?.hasAttribute('checked')).toBe(false);
    expect(toggle(element, 'agent')?.hasAttribute('checked')).toBe(true);
  });

  it('turns agent alerts off without touching chat alerts', async () => {
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem('scion-push-chat-messages', 'true');
    localStorage.setItem('scion-push-agent-events', 'true');
    element = await createComponent(createFetchHandler({}));

    const agent = toggle(element, 'agent');
    agent.checked = false;
    agent.dispatchEvent(new CustomEvent('sl-change'));
    await settle(element);

    expect(localStorage.getItem('scion-push-agent-events')).toBe('false');
    expect(localStorage.getItem('scion-push-chat-messages')).toBe('true');
    expect(toggle(element, 'agent')?.hasAttribute('checked')).toBe(false);
    expect(toggle(element, 'chat')?.hasAttribute('checked')).toBe(true);
  });

  it('turns agent alerts on without touching chat alerts', async () => {
    (window as unknown as { Notification: unknown }).Notification = FakeNotification;
    localStorage.setItem('scion-push-chat-messages', 'false');
    localStorage.setItem('scion-push-agent-events', 'false');
    element = await createComponent(createFetchHandler({}));

    const agent = toggle(element, 'agent');
    agent.checked = true;
    agent.dispatchEvent(new CustomEvent('sl-change'));
    await settle(element);

    expect(localStorage.getItem('scion-push-agent-events')).toBe('true');
    expect(localStorage.getItem('scion-push-chat-messages')).toBe('false');
  });
});

describe('scion-page-profile-settings — view mode shortcuts', () => {
  let element: AnyEl = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({})));
    await import('./profile-settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    localStorage.clear();
    vi.restoreAllMocks();
  });

  function shortcutsSwitch(): AnyEl {
    return element.shadowRoot.querySelector('.view-mode-shortcuts-switch');
  }

  async function toggleTo(checked: boolean): Promise<void> {
    const sw = shortcutsSwitch();
    sw.checked = checked;
    sw.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await element.updateComplete;
  }

  it('shows the toggle on by default', async () => {
    element = await createComponent(createFetchHandler({}));

    expect(shadowText(element)).toContain('View mode shortcuts');
    expect(shadowText(element)).toContain('Switch between Console, Chat and Terminals');
    expect(shortcutsSwitch().hasAttribute('checked')).toBe(true);
  });

  it('stores the preference when turned off and back on', async () => {
    const { areViewModeShortcutsEnabled } = await import('../../client/view-mode-shortcuts.js');
    element = await createComponent(createFetchHandler({}));

    await toggleTo(false);
    expect(areViewModeShortcutsEnabled()).toBe(false);
    expect(shortcutsSwitch().hasAttribute('checked')).toBe(false);

    await toggleTo(true);
    expect(areViewModeShortcutsEnabled()).toBe(true);
  });

  it('reflects a stored off preference on load', async () => {
    const { setViewModeShortcutsEnabled } = await import('../../client/view-mode-shortcuts.js');
    setViewModeShortcutsEnabled(false);
    element = await createComponent(createFetchHandler({}));

    expect(shortcutsSwitch().hasAttribute('checked')).toBe(false);
  });

  it('makes no request when toggled', async () => {
    element = await createComponent(createFetchHandler({}));
    const before = (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.length;

    await toggleTo(false);
    await settle(element);

    expect((globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mock.calls.length).toBe(
      before
    );
  });

  describe('when the preference changes outside this page', () => {
    /** Writes the store directly and fires the storage event another tab's write would. */
    function writeFromOtherTab(key: string, value: string | null): void {
      if (value === null) {
        localStorage.removeItem(key);
      } else {
        localStorage.setItem(key, value);
      }
      window.dispatchEvent(
        new StorageEvent('storage', { key, newValue: value, storageArea: localStorage })
      );
    }

    it('updates the toggle when another tab changes the preference', async () => {
      element = await createComponent(createFetchHandler({}));
      expect(shortcutsSwitch().hasAttribute('checked')).toBe(true);

      writeFromOtherTab('scion-view-mode-shortcuts', 'false');
      await element.updateComplete;
      expect(shortcutsSwitch().hasAttribute('checked')).toBe(false);

      writeFromOtherTab('scion-view-mode-shortcuts', null);
      await element.updateComplete;
      expect(shortcutsSwitch().hasAttribute('checked')).toBe(true);
    });

    it('updates the toggle when the preference changes elsewhere in this tab', async () => {
      const { setViewModeShortcutsEnabled } = await import('../../client/view-mode-shortcuts.js');
      element = await createComponent(createFetchHandler({}));

      setViewModeShortcutsEnabled(false);
      await element.updateComplete;
      expect(shortcutsSwitch().hasAttribute('checked')).toBe(false);

      setViewModeShortcutsEnabled(true);
      await element.updateComplete;
      expect(shortcutsSwitch().hasAttribute('checked')).toBe(true);
    });

    it('ignores a storage event for an unrelated key', async () => {
      element = await createComponent(createFetchHandler({}));

      // Change the stored value without its own event, then fire an event
      // for a different key: the toggle must not re-read the preference.
      localStorage.setItem('scion-view-mode-shortcuts', 'false');
      writeFromOtherTab('some-other-key', 'x');
      await element.updateComplete;
      expect(shortcutsSwitch().hasAttribute('checked')).toBe(true);
    });

    it('removes the same listeners it added once disconnected', async () => {
      const { VIEW_MODE_SHORTCUTS_CHANGED_EVENT } =
        await import('../../client/view-mode-shortcuts.js');
      const addSpy = vi.spyOn(window, 'addEventListener');
      element = await createComponent(createFetchHandler({}));
      const added = (type: string): unknown => addSpy.mock.calls.find(([t]) => t === type)?.[1];
      const storageHandler = added('storage');
      const changedHandler = added(VIEW_MODE_SHORTCUTS_CHANGED_EVENT);
      expect(storageHandler).toBeTypeOf('function');
      expect(changedHandler).toBeTypeOf('function');

      const removeSpy = vi.spyOn(window, 'removeEventListener');
      element.remove();
      element = null;
      const removed = (type: string): unknown[] =>
        removeSpy.mock.calls.filter(([t]) => t === type).map(([, h]) => h);
      expect(removed('storage')).toContain(storageHandler);
      expect(removed(VIEW_MODE_SHORTCUTS_CHANGED_EVENT)).toContain(changedHandler);
    });
  });
});

describe('scion-page-profile-settings — preselect recommended choices (ptone/scion#7842)', () => {
  let element: AnyEl = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({})));
    await import('./profile-settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    const features = { ...window.__SCION_FEATURES__ };
    delete features[DECISIONS_FLAG];
    window.__SCION_FEATURES__ = features;
    setPreselectRecommendedChoices(false);
    vi.restoreAllMocks();
  });

  const preselectSwitch = (el: AnyEl): AnyEl =>
    el.shadowRoot?.querySelector('sl-switch.preselect-recommended-switch');

  const authMe = (preselect?: boolean): AuthMeFixture => ({
    body: {
      ...makeAuthMe(),
      preferences: preselect === undefined ? {} : { preselectRecommendedChoices: preselect },
    },
  });

  it('is hidden while web.decisions is off', async () => {
    setFeatureFlag(DECISIONS_FLAG, false);
    element = await createComponent(createFetchHandler({}, undefined, authMe(true)));
    expect(preselectSwitch(element)).toBeNull();
    expect(shadowText(element)).not.toContain('Preselect recommended choices');
  });

  it('is off by default, renders the Decisions card heading, and shows the stored preference', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);
    element = await createComponent(createFetchHandler({}, undefined, authMe()));
    const titles = Array.from(element.shadowRoot?.querySelectorAll('h2.section-title') ?? []).map(
      (h) => (h as HTMLElement).textContent?.trim() ?? ''
    );
    expect(titles).toContain('Decisions');
    expect(titles).not.toContain('Choices');
    expect(preselectSwitch(element)?.hasAttribute('checked')).toBe(false);
    element.remove();

    element = await createComponent(createFetchHandler({}, undefined, authMe(true)));
    expect(preselectSwitch(element)?.hasAttribute('checked')).toBe(true);
  });

  it('saves with a per-key preferences PATCH and applies it to open cards', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);
    let sent: Record<string, unknown> | null = null;
    element = await createComponent(
      createFetchHandler({}, undefined, authMe(), (body) => {
        sent = body;
        return { status: 200, body: { id: 'u1' } };
      })
    );
    const sw = preselectSwitch(element);
    sw.checked = true;
    sw.dispatchEvent(new CustomEvent('sl-change'));
    await settle(element);
    await settle(element);

    expect(sent).toEqual({ preferences: { preselectRecommendedChoices: true } });
    expect(preselectRecommendedChoices()).toBe(true);
    expect(preselectSwitch(element)?.hasAttribute('checked')).toBe(true);
  });

  it('reverts the switch and shows the error when the PATCH fails', async () => {
    setFeatureFlag(DECISIONS_FLAG, true);
    element = await createComponent(
      createFetchHandler({}, undefined, authMe(), () => ({
        status: 500,
        body: { error: { code: 'INTERNAL', message: 'boom' } },
      }))
    );
    const sw = preselectSwitch(element);
    sw.checked = true;
    sw.dispatchEvent(new CustomEvent('sl-change'));
    await settle(element);
    await settle(element);

    expect(preselectRecommendedChoices()).toBe(false);
    expect(preselectSwitch(element)?.hasAttribute('checked')).toBe(false);
    expect(shadowText(element)).toContain('boom');
  });
});

describe('scion-page-profile-settings — execution container (ptone/scion#8819)', () => {
  let element: AnyEl = null;

  beforeAll(async () => {
    vi.stubGlobal('fetch', vi.fn(createFetchHandler({})));
    await import('./profile-settings.js');
  });

  afterEach(() => {
    element?.remove();
    element = null;
    const features = { ...window.__SCION_FEATURES__ };
    delete features[EXEC_CONTAINERS_FLAG];
    window.__SCION_FEATURES__ = features;
    vi.restoreAllMocks();
  });

  interface ExecContainerMockOpts {
    getStatus?: number;
    getBody?: Record<string, unknown>;
    putHandler?: (body: Record<string, unknown>) => {
      status: number;
      body: Record<string, unknown>;
    };
    deleteHandler?: () => { status: number; body?: Record<string, unknown> };
    agents?: Array<Record<string, unknown>>;
    agentsStatus?: number;
  }

  function makeExecFetch(opts: ExecContainerMockOpts = {}) {
    const base = createFetchHandler({});
    return (url: string | URL | Request, init?: RequestInit): Promise<Response> => {
      const path = typeof url === 'string' ? url : url instanceof URL ? url.pathname : url.url;
      const json = (status: number, body: unknown): Promise<Response> =>
        Promise.resolve(
          new Response(status === 204 ? null : JSON.stringify(body), {
            status,
            headers: status === 204 ? {} : { 'Content-Type': 'application/json' },
          })
        );

      if (path.endsWith('/api/v1/users/me/exec-container')) {
        if (init?.method === 'PUT') {
          const parsed = JSON.parse(init.body as string) as Record<string, unknown>;
          const res = opts.putHandler
            ? opts.putHandler(parsed)
            : { status: 200, body: { configured: true, state: 'ready' } };
          return json(res.status, res.body);
        }
        if (init?.method === 'DELETE') {
          const res = opts.deleteHandler ? opts.deleteHandler() : { status: 204 };
          return json(res.status, res.body ?? {});
        }
        return json(opts.getStatus ?? 200, opts.getBody ?? { configured: false });
      }
      if (path.startsWith('/api/v1/agents')) {
        if (opts.agentsStatus && opts.agentsStatus >= 400) {
          return json(opts.agentsStatus, { error: { message: 'backend unavailable' } });
        }
        return json(200, { agents: opts.agents ?? [] });
      }
      return base(url, init);
    };
  }

  const card = (el: AnyEl): AnyEl =>
    el.shadowRoot?.querySelector('[data-testid="exec-container-card"]');

  it('is hidden while hub.exec_containers is off and makes no exec-container API request', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, false);
    element = await createComponent(makeExecFetch());

    expect(card(element)).toBeNull();
    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
    const urls = (fetchMock.mock.calls as Array<[unknown]>).map(([u]) => String(u));
    expect(urls.some((u) => u.includes('/exec-container'))).toBe(false);
    expect(urls.some((u) => u.includes('/api/v1/agents'))).toBe(false);
  });

  it('renders unconfigured state with shell-only candidates and link to start a new one', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    element = await createComponent(
      makeExecFetch({
        getBody: { configured: false },
        agents: [
          {
            id: 'a-shell-1',
            name: 'ops-shell',
            projectId: 'p1',
            project: 'infra',
            harnessConfig: 'shell',
            phase: 'running',
            _capabilities: { actions: ['read', 'attach'] },
          },
          {
            id: 'a-shell-stopped',
            name: 'backup-shell',
            projectId: 'p1',
            project: 'infra',
            harnessConfig: 'shell',
            phase: 'stopped',
            _capabilities: { actions: ['read', 'attach'] },
          },
          {
            id: 'a-llm',
            name: 'claude-bot',
            projectId: 'p1',
            project: 'infra',
            harnessConfig: 'claude',
            phase: 'running',
            _capabilities: { actions: ['read', 'attach'] },
          },
          {
            id: 'a-no-attach',
            name: 'foreign-shell',
            projectId: 'p2',
            project: 'other',
            harnessConfig: 'shell',
            phase: 'running',
            _capabilities: { actions: ['read'] },
          },
        ],
      })
    );

    expect(card(element)).not.toBeNull();
    const text = shadowText(element);
    expect(text).toContain('Execution container');
    expect(text).toContain('$HOME');
    expect(text).toContain('/workspace');

    const options = Array.from(
      element.shadowRoot.querySelectorAll('.exec-container-select sl-option')
    ) as HTMLElement[];
    expect(options.map((o) => o.getAttribute('value'))).toEqual(['a-shell-stopped', 'a-shell-1']);

    const createLink = element.shadowRoot.querySelector(
      '[data-testid="exec-container-create-link"]'
    );
    expect(createLink?.getAttribute('href')).toBe('/agents/new?harness=shell');
  });

  it('registers a container via PUT and shows configured state', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    let putBody: Record<string, unknown> | null = null;
    element = await createComponent(
      makeExecFetch({
        getBody: { configured: false },
        agents: [
          {
            id: '11111111-1111-1111-1111-111111111111',
            name: 'ops-shell',
            projectId: 'p1',
            project: 'infra',
            harnessConfig: 'shell',
            phase: 'running',
          },
        ],
        putHandler: (body) => {
          putBody = body;
          return {
            status: 200,
            body: {
              configured: true,
              state: 'ready',
              agent: {
                id: '11111111-1111-1111-1111-111111111111',
                slug: 'ops-shell',
                name: 'ops-shell',
                projectId: 'p1',
                projectName: 'infra',
                phase: 'running',
                activity: 'idle',
              },
            },
          };
        },
      })
    );

    const sel = element.shadowRoot.querySelector('.exec-container-select') as AnyEl;
    sel.value = '11111111-1111-1111-1111-111111111111';
    sel.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await settle(element);
    await settle(element);

    expect(putBody).toEqual({ agentId: '11111111-1111-1111-1111-111111111111' });
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-configured"]')
    ).not.toBeNull();
    expect(shadowText(element)).toContain('ops-shell');
    expect(shadowText(element)).toContain('infra');
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-state"]')?.textContent?.trim()
    ).toBe('Ready');
    // homePersistent absent -> warning hidden
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-home-warning"]')
    ).toBeNull();
  });

  it('renders the uniform 422 message as returned by PUT', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    const uniformMsg =
      "That agent can't be used as your default execution container. It must be an execution container you can attach to, in a project you can access.";
    element = await createComponent(
      makeExecFetch({
        getBody: { configured: false },
        agents: [
          {
            id: '22222222-2222-2222-2222-222222222222',
            name: 'ops-shell',
            projectId: 'p1',
            harnessConfig: 'shell',
            phase: 'running',
          },
        ],
        putHandler: () => ({
          status: 422,
          body: { error: { code: 'exec_container_ineligible', message: uniformMsg } },
        }),
      })
    );

    const sel = element.shadowRoot.querySelector('.exec-container-select') as AnyEl;
    sel.value = '22222222-2222-2222-2222-222222222222';
    sel.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await settle(element);
    await settle(element);

    const errEl = element.shadowRoot.querySelector('[data-testid="exec-container-error"]');
    expect(errEl?.textContent).toContain(uniformMsg);
  });

  it('shows home-persistence warning only when homePersistent is explicitly false', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    element = await createComponent(
      makeExecFetch({
        getBody: {
          configured: true,
          state: 'suspended',
          agent: {
            id: 'a-1',
            slug: 'ephemeral-ops',
            name: 'ephemeral-ops',
            projectId: 'p1',
            projectName: 'cloud-proj',
            phase: 'suspended',
            homePersistent: false,
          },
        },
      })
    );

    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-state"]')?.textContent?.trim()
    ).toBe('Suspended');
    const warn = element.shadowRoot.querySelector('[data-testid="exec-container-home-warning"]');
    expect(warn).not.toBeNull();
    expect(warn?.textContent?.replace(/\s+/g, ' ')).toContain('logins will not survive a restart');

    // When homePersistent is true, warning must not render.
    element.remove();
    element = await createComponent(
      makeExecFetch({
        getBody: {
          configured: true,
          state: 'ready',
          agent: {
            id: 'a-1',
            slug: 'persist-ops',
            name: 'persist-ops',
            projectId: 'p1',
            projectName: 'local-proj',
            phase: 'running',
            homePersistent: true,
          },
        },
      })
    );
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-home-warning"]')
    ).toBeNull();
  });

  it('clears a configured container via DELETE and returns to unconfigured state', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    let deleted = false;
    element = await createComponent(
      makeExecFetch({
        getBody: {
          configured: true,
          state: 'stopped',
          agent: {
            id: 'a-1',
            slug: 'ops-shell',
            name: 'ops-shell',
            projectId: 'p1',
            projectName: 'infra',
            phase: 'stopped',
          },
        },
        deleteHandler: () => {
          deleted = true;
          return { status: 204 };
        },
      })
    );

    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-state"]')?.textContent?.trim()
    ).toBe('Stopped');

    const clearBtn = element.shadowRoot.querySelector('[data-testid="exec-container-clear"]');
    clearBtn.click();
    await settle(element);
    await settle(element);

    expect(deleted).toBe(true);
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-configured"]')
    ).toBeNull();
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-picker"]')
    ).not.toBeNull();
  });

  it('renders unavailable state with generic notice and Clear button', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    const reason = 'Your registered execution container is no longer available. Register another.';
    element = await createComponent(
      makeExecFetch({
        getBody: {
          configured: true,
          state: 'unavailable',
          reason,
        },
      })
    );

    const notice = element.shadowRoot.querySelector('[data-testid="exec-container-unavailable"]');
    expect(notice?.textContent).toContain(reason);
    expect(notice?.getAttribute('role')).toBe('alert');
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-configured"]')
    ).toBeNull();
    expect(element.shadowRoot.querySelector('[data-testid="exec-container-clear"]')).not.toBeNull();
  });

  it('requests server-side harnessConfig=shell filter when loading candidates', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    element = await createComponent(makeExecFetch({ getBody: { configured: false }, agents: [] }));

    const fetchMock = globalThis.fetch as unknown as ReturnType<typeof vi.fn>;
    const agentCalls = (fetchMock.mock.calls as Array<[unknown]>)
      .map(([u]) => String(u))
      .filter((u) => u.startsWith('/api/v1/agents'));
    expect(agentCalls).toHaveLength(1);
    expect(agentCalls[0]).toContain('harnessConfig=shell');
  });

  it('renders an alert error instead of "No execution containers available" when candidate fetch fails', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    element = await createComponent(
      makeExecFetch({ getBody: { configured: false }, agentsStatus: 500 })
    );

    const errEl = element.shadowRoot.querySelector('[data-testid="exec-container-error"]');
    expect(errEl).not.toBeNull();
    expect(errEl?.getAttribute('role')).toBe('alert');
    expect(errEl?.textContent).toContain('Could not load execution containers');
    expect(element.shadowRoot.querySelector('.exec-container-select')).toBeNull();
    expect(shadowText(element)).not.toContain('No execution containers available');
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-candidates-retry"]')
    ).not.toBeNull();
  });

  it('renders only the error and Retry (not the picker) when GET /users/me/exec-container fails', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    element = await createComponent(
      makeExecFetch({
        getStatus: 500,
        getBody: { error: { message: 'server error loading exec container' } },
      })
    );

    const errEl = element.shadowRoot.querySelector('[data-testid="exec-container-error"]');
    expect(errEl).not.toBeNull();
    expect(errEl?.getAttribute('role')).toBe('alert');
    expect(errEl?.textContent).toContain('server error loading exec container');
    expect(element.shadowRoot.querySelector('[data-testid="exec-container-retry"]')).not.toBeNull();
    expect(element.shadowRoot.querySelector('[data-testid="exec-container-picker"]')).toBeNull();
  });

  it('supports Change -> pick -> PUT from the configured state', async () => {
    setFeatureFlag(EXEC_CONTAINERS_FLAG, true);
    let putBody: Record<string, unknown> | null = null;
    element = await createComponent(
      makeExecFetch({
        getBody: {
          configured: true,
          state: 'ready',
          agent: {
            id: '11111111-1111-1111-1111-111111111111',
            slug: 'ops-1',
            name: 'ops-1',
            projectId: 'p1',
            projectName: 'infra',
            phase: 'running',
            activity: 'idle',
          },
        },
        agents: [
          {
            id: '11111111-1111-1111-1111-111111111111',
            name: 'ops-1',
            projectId: 'p1',
            project: 'infra',
            harnessConfig: 'shell',
            phase: 'running',
          },
          {
            id: '22222222-2222-2222-2222-222222222222',
            name: 'ops-2',
            projectId: 'p2',
            project: 'staging',
            harnessConfig: 'shell',
            phase: 'stopped',
          },
        ],
        putHandler: (body) => {
          putBody = body;
          return {
            status: 200,
            body: {
              configured: true,
              state: 'stopped',
              agent: {
                id: '22222222-2222-2222-2222-222222222222',
                slug: 'ops-2',
                name: 'ops-2',
                projectId: 'p2',
                projectName: 'staging',
                phase: 'stopped',
              },
            },
          };
        },
      })
    );

    const changeBtn = element.shadowRoot.querySelector('[data-testid="exec-container-change"]');
    changeBtn.click();
    await settle(element);
    await settle(element);

    const sel = element.shadowRoot.querySelector('.exec-container-select') as AnyEl;
    expect(sel).not.toBeNull();
    sel.value = '22222222-2222-2222-2222-222222222222';
    sel.dispatchEvent(new CustomEvent('sl-change', { bubbles: true, composed: true }));
    await settle(element);
    await settle(element);

    expect(putBody).toEqual({ agentId: '22222222-2222-2222-2222-222222222222' });
    expect(shadowText(element)).toContain('ops-2');
    expect(shadowText(element)).toContain('staging');
    expect(
      element.shadowRoot.querySelector('[data-testid="exec-container-state"]')?.textContent?.trim()
    ).toBe('Stopped');
    expect(element.shadowRoot.querySelector('[data-testid="exec-container-picker"]')).toBeNull();
  });
});
