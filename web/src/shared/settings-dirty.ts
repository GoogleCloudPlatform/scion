/**
 * Section-level change detection for the admin settings saves
 * (ptone/scion#3922). A page keeps the payload it would send right after it
 * loaded (the baseline) and the revision of each settings section; a save
 * then sends only the sections the user changed, each whole, with
 * expected_revisions so the hub refuses a section another admin changed
 * since the page loaded (409 revision_conflict) instead of overwriting it.
 *
 * Payload paths are koanf paths: a nested key path such as
 * server.hub.public_url is the koanf key, as the hub's section registry
 * names them (GET /api/v1/admin/server-config/schema, sectionPathsFromSchema).
 */

type Json = unknown;

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * The leaves of a payload, keyed by dotted path. Descent stops at a path that
 * is itself a koanf path of a section (so a list such as admin_emails, or a
 * map section such as runtimes, is one leaf) and at any value that is not a
 * plain object.
 */
export function payloadLeaves(
  payload: Record<string, unknown>,
  koanfPaths: ReadonlySet<string>
): Map<string, Json> {
  const out = new Map<string, Json>();
  const walk = (value: unknown, path: string): void => {
    if (path !== '' && (koanfPaths.has(path) || !isPlainObject(value))) {
      out.set(path, value);
      return;
    }
    if (!isPlainObject(value)) return;
    for (const [k, v] of Object.entries(value)) {
      walk(v, path === '' ? k : `${path}.${k}`);
    }
  };
  walk(payload, '');
  return out;
}

/**
 * The section a leaf belongs to: the section whose koanf path is the leaf, a
 * prefix of it, or (for a leaf above its koanf paths) below it; the longest
 * match wins. null when no section claims it.
 */
export function sectionOfPath(
  path: string,
  sectionKoanfPaths: Record<string, readonly string[]>
): string | null {
  let best: string | null = null;
  let bestLen = -1;
  for (const [section, paths] of Object.entries(sectionKoanfPaths)) {
    for (const kp of paths) {
      const match = kp === path || path.startsWith(`${kp}.`) || kp.startsWith(`${path}.`);
      if (match && kp.length > bestLen) {
        best = section;
        bestLen = kp.length;
      }
    }
  }
  return best;
}

/** Sets value at a dotted path, creating the parent objects. */
function setAtPath(target: Record<string, unknown>, path: string, value: unknown): void {
  const segs = path.split('.');
  let node = target;
  for (const seg of segs.slice(0, -1)) {
    if (!isPlainObject(node[seg])) node[seg] = {};
    node = node[seg] as Record<string, unknown>;
  }
  const last = segs[segs.length - 1];
  if (last !== undefined) node[last] = value;
}

/** Stable JSON for comparison (object keys sorted at every level). */
function stable(value: unknown): string {
  if (Array.isArray(value)) return `[${value.map(stable).join(',')}]`;
  if (isPlainObject(value)) {
    return `{${Object.keys(value)
      .sort()
      .map((k) => `${JSON.stringify(k)}:${stable(value[k])}`)
      .join(',')}}`;
  }
  return JSON.stringify(value ?? null);
}

/**
 * The revision to send for each section, from GET section_metadata: the
 * stored revision for a section with a row (source "db"), 0 for a section
 * with no row (create-only: a row created meanwhile conflicts).
 */
export function revisionsFromMetadata(
  metadata: Record<string, { source?: string; revision?: number }> | null | undefined
): Record<string, number> {
  const out: Record<string, number> = {};
  for (const [section, meta] of Object.entries(metadata ?? {})) {
    if (!meta) continue;
    out[section] = meta.source === 'db' && typeof meta.revision === 'number' ? meta.revision : 0;
  }
  return out;
}

export interface DirtyPayload {
  /** The leaves of every changed section (whole) and every changed unmapped leaf. */
  payload: Record<string, unknown>;
  /** Changed sections, sorted. */
  sections: string[];
  /** Revision per changed section that has a known revision. */
  expectedRevisions: Record<string, number>;
}

/**
 * Compares the payload a save would send with the baseline taken at load,
 * section by section, and returns only the changed sections (each with all
 * of its current leaves) plus their expected revisions.
 */
export function dirtySectionsPayload(
  current: Record<string, unknown>,
  baseline: Record<string, unknown>,
  sectionKoanfPaths: Record<string, readonly string[]>,
  revisions: Record<string, number>
): DirtyPayload {
  const koanf = new Set(Object.values(sectionKoanfPaths).flat());
  const now = payloadLeaves(current, koanf);
  const before = payloadLeaves(baseline, koanf);

  const bySection = new Map<string | null, { now: Map<string, Json>; before: Map<string, Json> }>();
  const bucket = (s: string | null) => {
    let b = bySection.get(s);
    if (!b) {
      b = { now: new Map(), before: new Map() };
      bySection.set(s, b);
    }
    return b;
  };
  for (const [p, v] of now) bucket(sectionOfPath(p, sectionKoanfPaths)).now.set(p, v);
  for (const [p, v] of before) bucket(sectionOfPath(p, sectionKoanfPaths)).before.set(p, v);

  const payload: Record<string, unknown> = {};
  const sections: string[] = [];
  for (const [section, b] of bySection) {
    if (section === null) {
      // Leaves no section claims: send each one that changed or appeared.
      // A leaf that disappeared since load is not sent (it has no value to
      // send); the page's payload builder never drops an unmapped leaf
      // today, so this does not arise.
      for (const [p, v] of b.now) {
        if (!b.before.has(p) || stable(b.before.get(p)) !== stable(v)) setAtPath(payload, p, v);
      }
      continue;
    }
    const changed =
      b.now.size !== b.before.size ||
      [...b.now].some(([p, v]) => !b.before.has(p) || stable(b.before.get(p)) !== stable(v));
    if (!changed) continue;
    sections.push(section);
    for (const [p, v] of b.now) setAtPath(payload, p, v);
  }
  sections.sort();
  const expectedRevisions: Record<string, number> = {};
  for (const s of sections) {
    const rev = revisions[s];
    if (typeof rev === 'number') expectedRevisions[s] = rev;
  }
  return { payload, sections, expectedRevisions };
}
