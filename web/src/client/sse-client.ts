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

/**
 * SSE Client
 *
 * Manages an EventSource connection to the server's /events endpoint.
 * Subscriptions are declared as query parameters at connection time and
 * are immutable for the connection lifetime. To change subscriptions,
 * disconnect and reconnect with different subjects.
 *
 * Provides automatic reconnection with exponential backoff and
 * Last-Event-ID resume support (handled natively by EventSource).
 */

/** Data shape for SSE 'update' events from the server */
export interface SSEUpdateEvent {
  subject: string;
  data: unknown;
}

type SSEClientEventMap = {
  update: CustomEvent<SSEUpdateEvent>;
  connected: CustomEvent<{ connectionId: string; subjects: string[] }>;
  disconnected: CustomEvent<void>;
  reconnecting: CustomEvent<{ attempt: number }>;
};

export class SSEClient extends EventTarget {
  private eventSource: EventSource | null = null;
  private reconnectAttempts = 0;
  private maxReconnectAttempts = 10;
  private baseReconnectDelay = 1000;
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  private subjects: string[] = [];

  /**
   * Build the SSE URL with subscription subjects as query parameters.
   * Maps to the WatchRequest pattern.
   */
  private buildUrl(subjects: string[]): string {
    const params = subjects.map((s) => `sub=${encodeURIComponent(s)}`).join('&');
    return `/events?${params}`;
  }

  /**
   * Open a connection scoped to the given subjects.
   * Closes any existing connection first.
   */
  connect(subjects: string[]): void {
    this.disconnect();
    this.subjects = subjects;
    this.reconnectAttempts = 0;
    this.openConnection();
  }

  private openConnection(): void {
    if (this.subjects.length === 0) {
      return;
    }

    const url = this.buildUrl(this.subjects);
    this.eventSource = new EventSource(url);

    this.eventSource.onopen = () => {
      this.reconnectAttempts = 0;
      console.info('[SSE] Connected');
    };

    this.eventSource.onerror = () => {
      // EventSource fires error when connection drops.
      // Close and attempt manual reconnect with backoff.
      const readyState = this.eventSource?.readyState;
      if (this.eventSource) {
        this.eventSource.close();
        this.eventSource = null;
      }

      // If the connection was never opened (CONNECTING → error), the
      // server likely returned a non-200 (e.g. 302 redirect to login
      // after session invalidation). Probe the auth endpoint before
      // burning through reconnect attempts.
      if (readyState === EventSource.CONNECTING) {
        void this.checkAuthAndReconnect();
      } else {
        this.scheduleReconnect();
      }
    };

    // Handle state update events from the server
    this.eventSource.addEventListener('update', (event) => {
      try {
        const data = JSON.parse((event as MessageEvent).data) as SSEUpdateEvent;
        this.dispatchEvent(new CustomEvent('update', { detail: data }));
      } catch (err) {
        console.error('[SSE] Failed to parse update event:', err);
      }
    });

    // Handle server-initiated reconnect (e.g. before a clean shutdown).
    this.eventSource.addEventListener('reconnect', () => {
      this.reconnectAttempts = 0;
      this.eventSource?.close();
      this.eventSource = null;
      this.openConnection();
    });

    // Handle initial connection acknowledgement
    this.eventSource.addEventListener('connected', (event) => {
      try {
        const data = JSON.parse((event as MessageEvent).data) as {
          connectionId: string;
          subjects: string[];
        };
        console.info('[SSE] Connection established:', data.connectionId);
        this.dispatchEvent(new CustomEvent('connected', { detail: data }));
      } catch (err) {
        console.error('[SSE] Failed to parse connected event:', err);
      }
    });
  }

  /**
   * Check whether the session is still valid before reconnecting.
   * If the session was invalidated (e.g. signing key rotation),
   * redirect to the login page instead of retrying.
   */
  private async checkAuthAndReconnect(): Promise<void> {
    try {
      const resp = await fetch('/auth/me', { credentials: 'include' });
      if (resp.status === 401 || resp.redirected) {
        console.warn('[SSE] Session expired, redirecting to login');
        const returnTo = encodeURIComponent(window.location.pathname);
        window.location.href = `/login?error=session_expired&returnTo=${returnTo}`;
        return;
      }
    } catch {
      // Network error — fall through to normal reconnect.
    }
    this.scheduleReconnect();
  }

  private scheduleReconnect(): void {
    if (this.reconnectAttempts >= this.maxReconnectAttempts) {
      console.warn('[SSE] Max reconnect attempts reached, giving up');
      this.dispatchEvent(new CustomEvent('disconnected'));
      return;
    }

    this.reconnectAttempts++;
    const delay = this.baseReconnectDelay * Math.pow(2, this.reconnectAttempts - 1);
    // Cap delay at 30 seconds
    const cappedDelay = Math.min(delay, 30_000);

    console.info(`[SSE] Reconnecting in ${cappedDelay}ms (attempt ${this.reconnectAttempts})`);
    this.dispatchEvent(
      new CustomEvent('reconnecting', { detail: { attempt: this.reconnectAttempts } })
    );

    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.openConnection();
    }, cappedDelay);
  }

  /**
   * Close the SSE connection and cancel any pending reconnection.
   */
  disconnect(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }

    if (this.eventSource) {
      this.eventSource.close();
      this.eventSource = null;
    }

    this.subjects = [];
    this.reconnectAttempts = 0;
  }

  /** Whether the connection is currently open */
  get connected(): boolean {
    return this.eventSource?.readyState === EventSource.OPEN;
  }

  /** Current subscription subjects */
  get currentSubjects(): string[] {
    return this.subjects;
  }

  /** Number of reconnect attempts since last successful connection */
  get reconnectAttemptCount(): number {
    return this.reconnectAttempts;
  }

  // Typed addEventListener overloads
  addEventListener<K extends keyof SSEClientEventMap>(
    type: K,
    listener: (ev: SSEClientEventMap[K]) => void,
    options?: boolean | AddEventListenerOptions
  ): void;
  addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject,
    options?: boolean | AddEventListenerOptions
  ): void;
  addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject | ((ev: CustomEvent) => void),
    options?: boolean | AddEventListenerOptions
  ): void {
    super.addEventListener(type, listener as EventListenerOrEventListenerObject, options);
  }
}
