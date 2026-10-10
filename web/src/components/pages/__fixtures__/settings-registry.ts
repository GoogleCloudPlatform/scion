/**
 * Settings registry helpers for the admin settings payload tests
 * (ptone/scion#3924).
 *
 * The registry data is pkg/config/opsettings/testdata/web_registry_fixture.json,
 * generated from pkg/config/opsettings/registry.go (Registry) and koanf.go
 * (layer0Prefixes). TestWebRegistryFixture in that package fails when the
 * fixture is out of date; regenerate it with
 *   go test ./pkg/config/opsettings -run TestWebRegistryFixture -update-web-registry-fixture
 */
import fixture from '../../../../../pkg/config/opsettings/testdata/web_registry_fixture.json';

/** Registry section name to its koanf paths. */
export const LAYER1_SECTIONS: Record<string, string[]> = fixture.layer1_sections;

const LAYER1_PATHS: Set<string> = new Set(Object.values(LAYER1_SECTIONS).flat());
const LAYER0_PREFIXES: string[] = fixture.layer0_prefixes;
const FILE_KEYS: Set<string> = new Set(fixture.file_keys);

/**
 * The /api/v1/admin/server-config/schema body a hub with this registry
 * returns (koanf_paths only; the page does not read the schemas).
 */
export function registrySchemaResponse(): { sections: Record<string, { koanf_paths: string[] }> } {
  const sections: Record<string, { koanf_paths: string[] }> = {};
  for (const [name, paths] of Object.entries(LAYER1_SECTIONS)) {
    sections[name] = { koanf_paths: [...paths] };
  }
  return { sections };
}

/**
 * The registry section that owns a koanf key, or "" for none. Mirrors
 * opsettings.OwningSection: an exact match, else the nearest listed parent
 * (so "runtimes.docker.type" belongs to "runtimes").
 */
export function owningSection(koanfKey: string): string {
  for (let key = koanfKey; key !== ''; ) {
    for (const [name, paths] of Object.entries(LAYER1_SECTIONS)) {
      if (paths.includes(key)) return name;
    }
    const idx = key.lastIndexOf('.');
    if (idx < 0) break;
    key = key.slice(0, idx);
  }
  return '';
}

/** Whether the key is in a registry section's koanf paths (exact match). */
export function isListedLayer1Path(koanfKey: string): boolean {
  return LAYER1_PATHS.has(koanfKey);
}

/** Mirrors opsettings.isLayer0Key. */
export function isLayer0Key(koanfKey: string): boolean {
  return LAYER0_PREFIXES.some((p) => koanfKey === p || koanfKey.startsWith(p + '.'));
}

/** An unclassified top-level settings.yaml key a workstation hub writes. */
export function isFileKey(koanfKey: string): boolean {
  return FILE_KEYS.has(koanfKey);
}

/**
 * The koanf key of a server-config PUT body path. The body follows the koanf
 * layout except that federation is a top-level member whose keys live under
 * server.federation (serverConfigBodyKoanfKey in pkg/hub).
 */
export function bodyPathToKoanf(path: string[]): string {
  const key = path.join('.');
  return path[0] === 'federation' ? `server.${key}` : key;
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

/**
 * Every leaf path of a JSON body. Arrays, null, scalars and empty objects
 * are leaves.
 */
export function payloadLeaves(body: unknown, prefix: string[] = []): string[][] {
  if (!isPlainObject(body) || (Object.keys(body).length === 0 && prefix.length > 0)) {
    return [prefix];
  }
  return Object.entries(body).flatMap(([k, v]) => payloadLeaves(v, [...prefix, k]));
}

/** The value at a path, and whether every member along it is present. */
export function valueAtPath(body: unknown, path: string[]): { present: boolean; value: unknown } {
  let cur: unknown = body;
  for (const name of path) {
    if (!isPlainObject(cur) || !Object.prototype.hasOwnProperty.call(cur, name)) {
      return { present: false, value: undefined };
    }
    cur = cur[name];
  }
  return { present: true, value: cur };
}

/**
 * Which keys a save may send:
 *  - 'layer1': registry keys only (a hosted DB-backed hub);
 *  - 'settings': registry keys, Layer-0 keys and the unclassified
 *    settings.yaml keys (a workstation or file-mode hub, which write the
 *    non-registry part to settings.yaml).
 */
export type PayloadScope = 'layer1' | 'settings';

/**
 * The koanf keys of a server-config PUT body that the server's registry
 * does not know for the given scope. Empty means every key is known.
 */
export function unknownPayloadKeys(body: unknown, scope: PayloadScope): string[] {
  const unknown: string[] = [];
  for (const leaf of payloadLeaves(body)) {
    const key = bodyPathToKoanf(leaf);
    if (owningSection(key) !== '') continue;
    if (scope === 'settings' && (isLayer0Key(key) || isFileKey(key))) continue;
    unknown.push(key);
  }
  return unknown;
}
