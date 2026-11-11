# webperf C8: CodeMirror per-language chunks (ptone/scion#5351)

Base: `7570904` (upstream main, after C3). Part of the webperf epic (#5301),
C workstream. Builds on C3's bundle-budget script.

## What changed

- `web/src/components/shared/code-editor.ts` already loaded each language with
  a dynamic `import()`, but the `manualChunks` rule in `web/vite.config.ts`
  sent every `@codemirror/*` and `@lezer/*` module into the one `codemirror`
  chunk. So the first editor on any page downloaded all nine languages
  (783,936 B raw).
- `vite.config.ts` now gives each language its own chunk: `@codemirror/lang-X`
  and its parser `@lezer/X` both go to `lang-X`. `@lezer/lr`, the LR parser
  runtime that only the language parsers use, goes to a shared `lezer-lr`
  chunk. `@lezer/common` and `@lezer/highlight` stay in `codemirror` with the
  editor core. `lang-html` and `lang-markdown` import `lang-css` and
  `lang-javascript` statically, as their packages do.
- `code-editor.ts` exports `languageFor(language): Promise<Extension>`, a map
  of dynamic imports keyed by the language id that `getLanguageFromPath`
  returns. Callers pass ids, not extensions, so the key is the id. An id
  without support (plaintext, shell, toml) resolves to `[]` with no import. A
  failed import logs a warning and resolves to `[]`. It never rejects.
- The editor now opens with a language `Compartment` holding `[]`, shows plain
  text, and reconfigures once the language has loaded. The language download
  starts at the same time as the core. A `language` property change also
  reconfigures (before this change the editor ignored it). A request counter
  makes sure only the latest request applies: a slower earlier import, or one
  for an editor that was rebuilt or removed, is dropped. Approved VIS (minor):
  the first time a language is opened, colouring appears a few ms after the
  text.
- The only consumers are `<scion-code-editor>` users (file editor, chat code
  blocks, chat file preview, artifact pages). `web/src` has no static
  `@codemirror/lang-*` import.
- `check-bundle-budget.mjs` adds a `chunks` limit: per chunk-name prefix, a
  raw/gzip limit on every matching built JS chunk, and at least one match is
  required (`$`-keys are comments). `bundle-budget.json` sets
  `assets/codemirror-` to 420,000 B raw / 140,000 B gzip, and adds the nine
  `lang-*` chunks to `requiredChunks`. C3's checks are unchanged.

## Numbers (vite build)

| chunk | before raw | after raw |
|---|---|---|
| `main.js` | 217,690 | 217,690 |
| entry closure (gzip) | 135,922 | 135,924 |
| `codemirror-*.js` | 783,936 | 409,273 |

Language chunks (raw): javascript 84,682, rust 70,864, python 45,158,
markdown 44,228, go 30,846, html 28,726, css 27,236, yaml 12,406, json 2,069,
plus `lezer-lr` 26,439.

## Learned

- The design target of 350,000 B for `codemirror-*.js` assumed about 430 KB of
  languages. The languages actually measured about 350 KB. The core that is
  left (minified bytes: view 195k, state 47k, autocomplete 38k, language 38k,
  commands 28k, lezer/common 28k, search 21k, highlight 7.5k) is used in full
  when an editor initialises. webperf-lead approved a revised limit of
  420,000 B. Two alternatives were rejected: splitting the core into two
  chunks (saves no bytes and games the budget) and lazy-loading search and
  autocomplete (a behaviour change).
- To measure what a package adds to a minified chunk, walk the chunk's source
  map with `source-map-js` and add up generated-column spans per source. The
  map's `sourcesContent` lengths count unminified bytes and overstate the
  size about 2.5x.
- With vitest `vi.mock` factories, test isolation and the "fails against a
  static import" proof pull against each other. The acceptance test keeps a
  cumulative import record (no reset in `beforeEach`), and the throwing and
  slow mocks live in a separate file. A static-import variant then fails on
  the assertion (all nine factories ran) instead of failing to load the file.

## Tests

- `code-editor-language.test.ts`: `editor loads only the requested language`
  (a `.go` file triggers exactly one language import, the go one, and the
  editor ends up configured with it), plus a language without support imports
  nothing.
- `code-editor-language-race.test.ts`: a failed import keeps plain text with
  no error state, and a language change applies the new language while a
  slower earlier import does not win.
- Mutants checked at dev time: a static import of all languages fails the
  acceptance test (9 imports instead of `['go']`). Removing the stale-request
  guard fails the race test (`rust` instead of `python`).
- `check-bundle-budget.test.mjs`: `chunk limits apply to every chunk matching
  the prefix`, and `fails when no chunk matches a sized prefix`.

## Upstream review follow-up (fix1)

- `languageFor` now matches ids case-insensitively. Before C8, `language='Go'` also fell back to plain text, so this is a fix, not a regression repair. It also looks ids up as own keys only, because an id such as `constructor` used to resolve to `Object.prototype.constructor`.
- The fixed 20 ms waits in the negative tests are gone. Each test now waits for an event it can name:
  - The unknown-language test awaits `languageFor` directly. That call settles only after any import it starts, so the check is deterministic. It then awaits `updateComplete` for the editor.
  - The failed-import test waits until the editor's own failure has been logged.
  - The race test awaits the slow module itself, then runs one zero-delay task. That task drains the editor's remaining promise continuations.
- Each negative test was rechecked against a broken variant: case-sensitive lookup, toml mapped to a language, the own-key check removed, a failure that rethrows, the stale guard removed, and static imports. Every variant fails its test.
