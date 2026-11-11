// P0 contract fixture only. Selections are counters, never real PTY attaches.
export function createCoordinator() {
  const params = new URLSearchParams(location.search);
  const basePath =
    new URL(params.get('base') || '/', location.origin).pathname.replace(/\/+$/, '') || '/';
  const key = JSON.stringify([location.origin, basePath, params.get('account') || 'fixture-user']);
  const lockName = `terminal-owner:p0:${key}`;
  const supported = isSecureContext && !!navigator.locks && typeof BroadcastChannel === 'function';
  const channel = supported ? new BroadcastChannel(lockName) : null;
  const requests = new Map();
  const executed = new Map();
  const sessions = new Set();
  let generation = null;
  let owner = false;
  let stopped = false;
  let executions = 0;
  let release;
  let claiming;
  let paused = false;
  const deferred = [];

  const send = (message) => channel?.postMessage({ ...message, key });
  const snapshot = () => ({
    key,
    lockName,
    supported,
    owner,
    generation,
    executions,
    sessions: [...sessions],
  });

  // The callback remains pending for the owner's document lifetime. No lease,
  // heartbeat expiration, lock stealing, or timeout-based ownership transfer.
  function claim() {
    if (owner) return Promise.resolve(true);
    if (claiming) return claiming;
    claiming = new Promise((resolve, reject) => {
      navigator.locks
        .request(lockName, { mode: 'exclusive', ifAvailable: true }, async (lock) => {
          if (!lock || stopped) {
            resolve(false);
            return;
          }
          owner = true;
          generation = crypto.randomUUID();
          document.title = 'Terminal workspace owner — fixture';
          const lifetime = new Promise((done) => {
            release = done;
          });
          resolve(true);
          await lifetime;
        })
        .catch(reject);
    }).finally(() => {
      claiming = null;
    });
    return claiming;
  }

  async function focusResult() {
    // Deterministic boundary injection tests the denied/unconfirmed UI contract.
    if (params.has('denyFocus')) return 'not-confirmed';
    try {
      window.focus();
      await new Promise((resolve) => setTimeout(resolve, 100));
      return document.hasFocus() && document.visibilityState === 'visible'
        ? 'document-focused'
        : 'not-confirmed';
    } catch {
      return 'error';
    }
  }

  function execute(message) {
    if (!owner || stopped || message.generation !== generation) return;
    if (paused) {
      deferred.push(message);
      return;
    }
    const existing = executed.get(message.requestId);
    if (existing && existing.agentId !== message.agentId) {
      deliver({ ...message, type: 'ack', status: 'request-id-conflict' });
      return;
    }
    if (!existing) {
      // Store the promise before execution starts: concurrent retries share it.
      const result = Promise.resolve().then(async () => {
        executions++;
        sessions.add(message.agentId);
        const focus = await focusResult();
        return {
          ...message,
          type: 'ack',
          status: 'selected',
          focus,
          text: 'Selected in your terminal workspace',
          desktopForeground: 'unverified',
        };
      });
      executed.set(message.requestId, { agentId: message.agentId, result });
    }
    void executed.get(message.requestId).result.then((ack) => {
      if (owner && !stopped) deliver(ack);
    });
  }

  function deliver(message) {
    receive(message); // BroadcastChannel does not echo to the sending object.
    send(message);
  }

  function receive(message) {
    if (stopped || message?.key !== key || typeof message.requestId !== 'string') return;
    if (message.type === 'discover' && owner) {
      send({ type: 'owner', requestId: message.requestId, generation });
    } else if (message.type === 'owner' && typeof message.generation === 'string') {
      const request = requests.get(message.requestId);
      if (!request || request.done) return;
      request.generation = message.generation;
      send({
        type: 'open',
        requestId: message.requestId,
        agentId: request.agentId,
        generation: message.generation,
      });
    } else if (message.type === 'open' && typeof message.agentId === 'string') {
      execute(message);
    } else if (message.type === 'ack') {
      const request = requests.get(message.requestId);
      if (
        !request ||
        request.done ||
        request.agentId !== message.agentId ||
        request.generation !== message.generation
      )
        return;
      request.done = true;
      request.resolve(message);
    }
  }
  if (channel) channel.onmessage = ({ data }) => receive(data);

  async function open(agentId, requestId, timeoutMs = 750) {
    if (!supported || stopped) return { status: 'unsupported', key };
    let request = requests.get(requestId);
    if (request && request.agentId !== agentId) return { status: 'request-id-conflict', requestId };
    if (!request) {
      request = { agentId, done: false };
      request.result = new Promise((resolve) => {
        request.resolve = resolve;
      });
      requests.set(requestId, request);
    }
    if (!request.done) {
      try {
        if (await claim()) {
          request.generation = generation;
          execute({ type: 'open', key, generation, agentId, requestId });
        } else {
          send({ type: 'discover', requestId });
        }
      } catch {
        return { status: 'unsupported', key };
      }
    }
    let timer;
    const pending = new Promise((resolve) => {
      timer = setTimeout(
        () =>
          resolve({
            status: 'pending',
            requestId,
            text: 'Owner unavailable or delayed; retry without opening a second workspace',
          }),
        timeoutMs
      );
    });
    return Promise.race([request.result, pending]).finally(() => clearTimeout(timer));
  }

  function stop() {
    stopped = true;
    sessions.clear(); // Production must close transports before releasing authority.
    owner = false;
    release?.();
    channel?.close();
  }
  addEventListener('pagehide', stop, { once: true });
  return {
    open,
    snapshot,
    stop,
    pause(value) {
      paused = value;
      if (!paused) deferred.splice(0).forEach(execute);
    },
  };
}
