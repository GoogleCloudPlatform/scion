// The fixture app's module script. It checks a same-app fetch and a
// WebSocket echo, both by relative URL, and writes the result to #probe:
// "ok" when both work, otherwise "fail:" and the failed checks.
const probe = document.getElementById('probe');

async function checkFetch() {
  const res = await fetch('./api/ping');
  if (!res.ok) throw new Error(`fetch ${res.status}`);
  const body = await res.json();
  if (body.pong !== true) throw new Error('fetch body');
}

function checkWebSocket() {
  return new Promise((resolve, reject) => {
    const url = new URL('./ws', document.baseURI);
    url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(url);
    const timer = setTimeout(() => reject(new Error('ws timeout')), 10000);
    ws.onopen = () => ws.send('echo-check');
    ws.onmessage = (ev) => {
      clearTimeout(timer);
      ws.close();
      if (ev.data === 'echo-check') resolve();
      else reject(new Error('ws echo'));
    };
    ws.onerror = () => {
      clearTimeout(timer);
      reject(new Error('ws error'));
    };
  });
}

const failed = [];
for (const [name, check] of [
  ['fetch', checkFetch],
  ['ws', checkWebSocket],
]) {
  try {
    await check();
  } catch (err) {
    failed.push(`${name}(${String(err && err.message ? err.message : err)})`);
  }
}
probe.textContent = failed.length === 0 ? 'ok' : `fail:${failed.join(',')}`;
