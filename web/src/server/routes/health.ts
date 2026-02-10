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
 * Health check routes
 *
 * Provides liveness and readiness probes for Kubernetes/Cloud Run
 */

import Router from '@koa/router';

const router = new Router();

interface HealthResponse {
  status: 'healthy' | 'unhealthy';
  timestamp: string;
  uptime: number;
}

/**
 * GET /healthz - Liveness probe
 *
 * Returns healthy if the server is running and can respond to requests.
 * This is used by Kubernetes/Cloud Run to determine if the container is alive.
 */
router.get('/healthz', (ctx) => {
  const response: HealthResponse = {
    status: 'healthy',
    timestamp: new Date().toISOString(),
    uptime: process.uptime(),
  };

  ctx.status = 200;
  ctx.type = 'application/json';
  ctx.body = response;
});

/**
 * GET /readyz - Readiness probe
 *
 * Returns healthy if the server is ready to serve traffic.
 * In the future, this will check database connections, NATS connectivity, etc.
 */
router.get('/readyz', (ctx) => {
  // TODO: Add checks for:
  // - Hub API connectivity
  // - NATS connection
  // - Session store

  const response: HealthResponse = {
    status: 'healthy',
    timestamp: new Date().toISOString(),
    uptime: process.uptime(),
  };

  ctx.status = 200;
  ctx.type = 'application/json';
  ctx.body = response;
});

export const healthRoutes = router;
