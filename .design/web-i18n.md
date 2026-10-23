# Web UI internationalization

Tracking: GoogleCloudPlatform/scion#4085. Initial slice; the translated UI is
experimental and incomplete.

## Scope and activation

`web.i18n` is registered as a default-off, web-only experiment. Enable it in
the existing administrator Experiments UI, then reload. The normal boot resolves
experiments before initializing localization and rendering the first route.
When disabled, the UI stays English and offers no language selector; a saved
language choice is retained for a later opt-in. No API behavior is gated by it.

The initial slice translates the application shell, sidebar, header actions,
mode menus, shell page titles, and known breadcrumb labels. The header provides
the same language selector at desktop and mobile widths. Page bodies, chat
shell-specific UI, onboarding, login and error pages, notifications' contents,
and documentation remain follow-up work. Entity names, URLs, API enum values,
commands, terminal output, logs, and agent-generated content are never translated.

## Implementation

`web/src/i18n/locales.ts` registers each locale's BCP 47 code, native name,
direction, accepted aliases and message-catalog loader. `index.ts` exposes a source-rendering facade; the opt-in `runtime.ts` owns the
active locale, preference persistence and document metadata, using an isolated
i18next instance from `engine.ts`.
The default-off shell uses the English source catalog without loading the engine.
After opt-in, boot loads the runtime, language selector, engine and all registered catalogs through local Vite
chunks before rendering. A shared promise prevents duplicate loads; an initialization
generation prevents an older load from overriding a newer disable/reinitialization.
A failed load leaves the page in English with the selector disabled. Subsequent
language switching is synchronous because the catalogs are already available.
Vite keeps non-English Shoelace translations in separate locale chunks; otherwise
its shared vendor chunk would load those translations even while the experiment is off.

i18next provides interpolation, CLDR plural forms and source-language fallback.
It was chosen over a bespoke dictionary engine to avoid encoding English/Chinese
plural assumptions. Lit's localization tooling is also viable, but this slice
uses semantic keys and ordinary catalogs rather than adding generated-template
and XLIFF build steps. Components depend on the small `t()`/controller interface.

Use `t('header.help')` inside `render()`. Do not evaluate translations into
module-level constants: navigation/page-title tables hold message keys, so
existing mounted views can change language. Add a `LocaleController` to every
component that renders translated strings. It requests an update on locale changes
and unregisters on disconnect. Non-template side effects, such as document titles,
can use the controller's optional callback. Existing detail-title strings are
preserved as opaque entity context; translating detail pages' own title vocabulary
belongs to those pages' later migration.

The preference is stored under `scion-language` for this browser origin (shared
by accounts and path prefixes on that origin, like the existing theme preference),
not sent to the Hub. Explicit choice wins; otherwise browser languages are tried in
order. Invalid/unsupported explicit preferences fall back to English. Auto removes
the preference. Storage failures leave the current tab usable. Storage events and
browser-language changes update connected components. Matching does not silently
turn `zh-TW`/`zh-Hant` into Simplified Chinese.

## Adding a language or message

1. Add a catalog under `web/src/i18n/locales/`, satisfying `MessageCatalog`.
2. Add a definition to `locales.ts` with the native name and `ltr`/`rtl` direction.
   Specify aliases deliberately; load its Shoelace translation in that loader when available.
3. Preserve message meanings and placeholder names. Use `count` plus CLDR suffixes
   (`_one`, `_other`, `_few`, etc.) for plurals. Translate whole sentences; translators
   may reorder placeholders. Do not concatenate translated sentence fragments.
4. Run `npm run typecheck`, `npm run lint`, `npm test`, and `npm run build` from `web/`.
   Catalog tests check registration, migrated-message coverage, plural categories,
   and placeholder parity. Runtime English fallback also handles temporarily missing
   or empty strings, but the shipped slice's catalogs must pass the coverage checks.
5. Visually check narrow layouts, long labels and accessibility names. `dir` metadata
   is supported, but complete RTL layout support requires checking physical CSS
   properties in each migrated surface; this slice does not claim RTL UI coverage.

The source catalog defines typed message keys. Source messages use named `{{value}}`
placeholders and cardinal plural suffixes only, which also work in the lightweight
English renderer used before the engine loads; nesting/markup expressions are not
part of that source contract. Add a message to English first,
then all shipped catalogs. Bind translated values as Lit text/attributes; never
pass them into `innerHTML` or `unsafeHTML`. i18next HTML escaping is disabled because
Lit performs escaping at the binding boundary (avoiding double escaping).

## Follow-up boundaries

- `utils/time.ts` deliberately fixes `FORMAT_LOCALE` to English. That constant also
  feeds numeric `formatToParts()` in wall-clock conversion: replacing it globally
  with a user locale can break parsing/calculations in languages using other digits
  or calendars. Separate machine-formatting from display-formatting first, preserve
  timezone/24-hour contracts, and include locale in display-formatter cache keys.
- Migrate page bodies in bounded groups, including toasts, dialogs, counts and
  accessibility labels; keep a clear inventory of untranslated surfaces.
- Assess persisted per-user language preferences only if cross-device sync is needed.
  The initial implementation requires no preference schema or API changes.
- Shoelace locale resources are adapted in the registry, not in feature components,
  so a future UI-library replacement has one localization integration point.
