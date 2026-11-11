import { createServer } from 'node:http';
import { build } from 'esbuild';

// Bundle the production coordinator. No Hub, broker, credentials or live agents.
const bundle = await build({
  entryPoints: [new URL('./fixture.ts', import.meta.url).pathname],
  bundle: true,
  write: false,
  format: 'esm',
  platform: 'browser',
});
const server = createServer((request, response) => {
  let path;
  try {
    path = new URL(request.url, 'http://localhost').pathname;
  } catch {
    response.writeHead(400).end();
    return;
  }
  if (path === '/fixture.js') {
    response.writeHead(200, { 'Content-Type': 'text/javascript' });
    response.end(bundle.outputFiles[0].text);
  } else if (path === '/') {
    response.writeHead(200, { 'Content-Type': 'text/html' });
    response.end(
      '<!doctype html><title>Coordinator fixture</title><main>Dashboard</main><script type="module" src="/fixture.js"></script>'
    );
  } else {
    response.writeHead(404).end();
  }
});
server.listen(4519, '127.0.0.1');
