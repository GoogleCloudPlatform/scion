# Web pages and apps through scion expose

Read SKILL.md in this skill first for `scion expose` itself. This file covers
pages and apps a person opens in their browser through the link.

A person opens your page in their own browser through a link on the Hub. What
that page can load depends on whether the Hub has its page view on:

- **Page view on**: the page's relative URLs load: separate scripts
  (including modules), stylesheets, images and fonts, and `fetch`, XHR and
  EventSource calls back to your app. See With the Hub's page view on.
- **Page view off**: the page can only use what is inside the page itself.
  Separate scripts, stylesheets, images and requests back to your app are
  not served.

You usually cannot tell from inside the agent which applies. If you do not
know, build **one self-contained HTML file**: it works either way. For a
full app, point the person to `scion tunnel` (below).

## Steps

1. Build the page as **one self-contained HTML file** (see Build settings),
   or, if the person confirms that the Hub's page view is on, as an app that
   uses only relative URLs (see With the Hub's page view on).
2. Serve it on `127.0.0.1` on a free port (1024 or above; 9810 and 18380
   cannot be exposed), as `index.html` so the root URL opens it:
   `python3 -m http.server <port> --bind 127.0.0.1 --directory <folder>`
3. Run: `scion expose <port> --label "<what it is>"`
4. Verify it (see Verifying honestly).
5. Give the human the URL **exactly as printed**, including the trailing `/`.
   Do not hand out tokens, and do not build the URL by hand.

## What works when a person opens the link

These hold whether or not the page view is on:

- Works: one HTML file with its CSS in `<style>`, its JavaScript in `<script>`
  (no `src`), and images and fonts as `data:` URLs.
- Does not work: URLs that start with `/` (for example `/app.js` or
  `/api/data`), in `src`, `href`, CSS `url()` and requests.
- Does not work: localStorage, sessionStorage, IndexedDB, cookies, web workers
  and service workers.
- Does not work: dev servers with live reload (`vite dev`, `next dev`).
- Does not work: client-side routing by path (React Router's
  `BrowserRouter`, Vue Router's `createWebHistory` and similar). Use hash
  routing instead (`HashRouter`, `createWebHashHistory`), or no router.

With the page view off, these also do not work: `<script src>`,
`<link rel="stylesheet" href>`, `<img src>` pointing at your server, and
`fetch`, XHR, EventSource or WebSocket calls back to your app, relative URLs
(`./app.js`) included. Data the page needs must then be inlined at build
time (for example a `<script type="application/json">` block or a JS
constant).

## With the Hub's page view on

When the person opens the link and the Hub can serve the agent's port for
them, the Hub sends them on to a short-lived page link, and the page is
served from there:

- Relative URLs work for scripts (including modules), stylesheets, images,
  fonts, and `fetch`, XHR and EventSource calls to your app. Write them as
  `./app.js` or `app.js`, never `/app.js`.
- Requests that send an `Authorization` header or cookies are not carried to
  your app; the page link itself is the access.
- A relative WebSocket (`new WebSocket(new URL('./ws', document.baseURI))`
  with the protocol changed to `ws:` or `wss:`) works when the agent is
  connected over the Hub's conduit transport. On the port-forward tunnel it
  is refused.
- Hash routing works. A path that the page changes with the history API is
  not kept on a reload outside the page link; use hash routing.
- Vite: set `base: './'` in `vite.config.*` so the built files refer to each
  other relatively, then `vite build` and serve `dist`.
- The page link grants access: do not log it, paste it into tickets or chats,
  or send it anywhere. Hand out only the URL `scion expose` printed.

## Full apps: scion tunnel

For a full multi-file app, a backend API or a dev server, the human can use
`scion tunnel` from their machine with the scion CLI, if their Hub has
`scion tunnel` turned on. Keep the port exposed with `scion expose <port>`
(the tunnel only reaches exposed ports), then suggest:

    scion tunnel <agent> --port <port>

The app then opens on a local port on their machine and works as it does
inside the agent. See
https://googlecloudplatform.github.io/scion/hosted/user/port-forwarding/#multi-file-apps

## Build settings (single-file output)

- **Vite** (React, Vue, Svelte, plain): add `vite-plugin-singlefile`
  (`npm i -D vite-plugin-singlefile`, then in `vite.config.*`
  `import { viteSingleFile } from 'vite-plugin-singlefile'` and
  `plugins: [..., viteSingleFile()]`) and run `vite build`.
  `dist/index.html` is the whole app: everything your code imports (CSS,
  images, fonts) is inlined. Serve `dist`. Files in `public/` that the page refers to by URL (for example
  `/favicon.svg` or `/data.json`) are NOT inlined and will not load through
  the link: import them from your code instead, or inline them by hand.
- **Plain HTML**: put the CSS in `<style>` and the JS in `<script>`. Turn
  images into data URLs, for example
  `echo "data:image/png;base64,$(base64 -w0 logo.png)"`.
- **Create React App, webpack, Parcel, esbuild**: none of them produce a
  single file without extra setup. The dependable route is to build the app
  with Vite and `vite-plugin-singlefile` (a React, Vue or Svelte app usually
  moves over with little change). For a small app, you can instead paste the
  built JS into a `<script>` and the CSS into a `<style>` in the HTML, turn
  images into data URLs, and check the result as below.
- **Next.js, server-rendered apps, apps with an API**: not supported through
  the link. Use `scion tunnel`, or produce a static single-file page with the
  data inlined.
- **Static servers**: `python3 -m http.server <port> --bind 127.0.0.1
  --directory <folder>` serves the folder as is. Any static server bound to
  `127.0.0.1` works; what matters is the single file it serves.

## Verifying honestly

- A `curl` with your own agent token is NOT proof. It only shows that the
  file is served; it does not behave like a person's browser and will succeed
  for pages that are broken for them.
- Search the built HTML for `src=`, `href=`, `url(`, `fetch(`, `XMLHttpRequest`,
  `EventSource`, `WebSocket`, `import(`, `localStorage`, `sessionStorage`,
  `indexedDB`, `document.cookie`, `new Worker`, `SharedWorker` and
  `serviceWorker`. Browser storage, cookies and workers will not work through
  the link. With the page view off, nothing that points at your server
  works either; with it on, relative URLs to your app work and URLs that
  start with `/` do not. Ignore matches inside minified library code and
  links to other sites; look for URLs that point at your own server.
- Search your SOURCE (not the build) for `BrowserRouter`,
  `createBrowserRouter` and `createWebHistory`: any hit is path-based
  routing, which will not work through the link.
- These two searches are the only check for storage, cookies, workers and
  path-based routing: they work on `127.0.0.1`, so the browser check below
  does not catch them.
- Load the page in a headless browser (for example Playwright Chromium) from
  `http://127.0.0.1:<port>/`, with every request other than the page itself
  blocked and recorded. In Playwright:

  ```js
  const blocked = [];
  await page.route('**/*', route => {
    const req = route.request();
    if (req.isNavigationRequest() && req.frame() === page.mainFrame()) return route.continue();
    blocked.push(req.url());
    return route.abort();
  });
  const errors = [];
  page.on('console', m => { if (m.type() === 'error') errors.push(m.text()); });
  page.on('pageerror', e => errors.push(String(e)));
  await page.goto('http://127.0.0.1:<port>/');
  // then: check what the page shows, and that blocked and errors are both empty
  ```

  Check that the page renders what it should, that `errors` is empty, and
  that `blocked` is empty. Anything in `blocked` is something the page loads
  from outside itself; inline it. For a relative-URL app built for the page
  view, serve it under a sub-path (for example
  `http://127.0.0.1:<port>/app/`) and let through only requests under that
  path: a URL that starts with `/` then shows up in `blocked`, as it would
  fail through the link.
- Report what you verified and how (which checks, which browser). If you
  could not run a browser, say so plainly instead of reporting the page as
  working.
