/**
 * Authentication routes
 *
 * Handles OAuth login flows for Google and GitHub providers
 */

import Router from '@koa/router';
import type { Context } from 'koa';
import crypto from 'crypto';

import type { AppConfig } from '../config.js';
import type { User } from '../../shared/types.js';
import { isEmailAuthorized } from '../middleware/auth.js';

/**
 * OAuth provider type
 */
type OAuthProvider = 'google' | 'github';

/** Debug flag for auth routes */
let authRoutesDebugEnabled = false;

/**
 * Debug logger for auth routes
 */
function authRoutesDebug(message: string, data?: Record<string, unknown>): void {
  if (!authRoutesDebugEnabled) return;
  const timestamp = new Date().toISOString();
  if (data) {
    console.log(`[AUTH-ROUTES ${timestamp}] ${message}`, JSON.stringify(data, null, 2));
  } else {
    console.log(`[AUTH-ROUTES ${timestamp}] ${message}`);
  }
}

/**
 * Create authentication routes
 */
export function createAuthRouter(config: AppConfig): Router {
  const router = new Router();

  // Enable debug if config says so
  if (config.debug) {
    authRoutesDebugEnabled = true;
  }

  /**
   * Generate random state for CSRF protection
   */
  function generateState(): string {
    return crypto.randomBytes(32).toString('hex');
  }

  /**
   * GET /auth/login
   * Show login page (redirect to specific provider or show options)
   */
  router.get('/login', async (ctx: Context) => {
    // Store return URL if provided
    const returnTo = ctx.query.returnTo as string | undefined;
    if (returnTo && ctx.session) {
      ctx.session.returnTo = returnTo;
    }

    // Redirect to login page component
    ctx.redirect('/login');
  });

  /**
   * GET /auth/login/:provider
   * Initiate OAuth flow for specified provider
   */
  router.get('/login/:provider', async (ctx: Context) => {
    const provider = ctx.params.provider as string;

    // Ensure we are on the canonical base URL before starting OAuth flow.
    // This is critical for session cookies to be available at the callback URL.
    if (config.baseUrl) {
      try {
        const baseUri = new URL(config.baseUrl);
        const currentHost = ctx.host;
        const currentProtocol = ctx.protocol;
        const expectedProtocol = baseUri.protocol.replace(':', '');

        if (currentHost !== baseUri.host || currentProtocol !== expectedProtocol) {
          authRoutesDebug(`Host or protocol mismatch, redirecting to canonical base URL`, {
            currentHost,
            baseHost: baseUri.host,
            currentProtocol,
            expectedProtocol,
          });
          const targetUrl = new URL(ctx.url, config.baseUrl);
          ctx.redirect(targetUrl.toString());
          return;
        }
      } catch (e) {
        authRoutesDebug(`Failed to parse config.baseUrl`, { baseUrl: config.baseUrl });
      }
    }

    // Store return URL if provided
    const returnTo = ctx.query.returnTo as string | undefined;
    if (returnTo && ctx.session) {
      ctx.session.returnTo = returnTo;
    }

    if (provider === 'google') {
      // Check if Google OAuth is configured
      if (!config.auth.googleClientId) {
        ctx.redirect('/auth/error?message=Google+OAuth+not+configured');
        return;
      }

      // Generate state for CSRF protection
      const state = generateState();
      if (ctx.session) {
        ctx.session.oauthState = state;
      }

      // Generate authorization URL
      const redirectUri = `${config.baseUrl}/auth/callback/google`;
      const params = new URLSearchParams({
        client_id: config.auth.googleClientId,
        redirect_uri: redirectUri,
        response_type: 'code',
        scope: 'openid email profile',
        state: state,
        access_type: 'offline',
        prompt: 'select_account',
      });

      const authUrl = `https://accounts.google.com/o/oauth2/v2/auth?${params.toString()}`;

      ctx.redirect(authUrl);
      return;
    }

    if (provider === 'github') {
      // Check if GitHub OAuth is configured
      if (!config.auth.githubClientId) {
        ctx.redirect('/auth/error?message=GitHub+OAuth+not+configured');
        return;
      }

      // Generate state for CSRF protection
      const state = generateState();
      if (ctx.session) {
        ctx.session.oauthState = state;
      }

      const redirectUri = `${config.baseUrl}/auth/callback/github`;
      const authUrl =
        `https://github.com/login/oauth/authorize?` +
        `client_id=${encodeURIComponent(config.auth.githubClientId)}` +
        `&redirect_uri=${encodeURIComponent(redirectUri)}` +
        `&scope=${encodeURIComponent('user:email')}` +
        `&state=${encodeURIComponent(state)}`;

      ctx.redirect(authUrl);
      return;
    }

    // Unknown provider
    ctx.redirect('/auth/error?message=Unknown+OAuth+provider');
  });

  /**
   * GET /auth/callback/:provider
   * Handle OAuth callback from provider
   */
  router.get('/callback/:provider', async (ctx: Context) => {
    const provider = ctx.params.provider as OAuthProvider;
    const code = ctx.query.code as string | undefined;
    const state = ctx.query.state as string | undefined;
    const error = ctx.query.error as string | undefined;

    authRoutesDebug(`OAuth callback received`, {
      provider,
      hasCode: !!code,
      hasState: !!state,
      hasError: !!error,
      sessionExists: !!ctx.session,
      sessionOauthState: ctx.session?.oauthState ? 'present' : 'missing',
    });

    // Check for OAuth errors
    if (error) {
      const errorDescription = (ctx.query.error_description as string) || 'Authentication failed';
      authRoutesDebug(`OAuth error from provider`, { error, errorDescription });
      ctx.redirect(`/auth/error?message=${encodeURIComponent(errorDescription)}`);
      return;
    }

    // Verify code is present
    if (!code) {
      authRoutesDebug(`Missing authorization code`);
      ctx.redirect('/auth/error?message=Missing+authorization+code');
      return;
    }

    // Verify state matches (CSRF protection)
    if (ctx.session?.oauthState !== state) {
      authRoutesDebug(`State mismatch`, {
        expectedState: ctx.session?.oauthState ? 'present' : 'missing',
        receivedState: state ? 'present' : 'missing',
        match: ctx.session?.oauthState === state,
      });
      ctx.redirect('/auth/error?message=Invalid+state+parameter');
      return;
    }

    // Clear the state from session
    if (ctx.session) {
      delete ctx.session.oauthState;
    }

    try {
      authRoutesDebug(`Exchanging code for Hub tokens`, { provider });

      // The redirect URI must match exactly what was sent to the provider
      const redirectUri = `${config.baseUrl}/auth/callback/${provider}`;

      // Call Hub API to exchange code for Hub-issued tokens (Option A from server-auth-design.md)
      // This is a single exchange that handles both code validation and Hub session creation.
      const hubTokenResponse = await fetch(`${config.hubApiUrl}/api/v1/auth/token`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
        },
        body: JSON.stringify({
          provider: provider,
          code: code,
          redirectUri: redirectUri,
          grantType: 'authorization_code',
          clientType: 'web',
        }),
      });

      if (!hubTokenResponse.ok) {
        const errorText = await hubTokenResponse.text();
        authRoutesDebug(`Hub token exchange failed`, {
          status: hubTokenResponse.status,
          error: errorText.substring(0, 200),
        });
        throw new Error(`Hub authentication failed: ${hubTokenResponse.statusText}`);
      }

      const hubTokenData = (await hubTokenResponse.json()) as {
        accessToken: string;
        refreshToken: string;
        expiresIn: number;
        user: { id: string; email: string; displayName: string; avatarUrl?: string; role: string };
      };

      const user: User = {
        id: hubTokenData.user.id,
        email: hubTokenData.user.email,
        name: hubTokenData.user.displayName,
        avatar: hubTokenData.user.avatarUrl,
      };

      authRoutesDebug(`User authenticated via Hub`, {
        userId: user.id,
        userEmail: user.email,
        hasHubToken: !!hubTokenData.accessToken,
      });

      // Check if user's email domain is authorized (secondary check, Hub might already do this)
      if (!isEmailAuthorized(user.email, config.auth.authorizedDomains)) {
        authRoutesDebug(`User email domain not authorized`, { email: user.email });
        ctx.redirect('/auth/error?message=Your+email+domain+is+not+authorized');
        return;
      }

      // Store user and Hub tokens in session
      if (ctx.session) {
        ctx.session.user = user;
        ctx.session.hubAccessToken = hubTokenData.accessToken;
        ctx.session.hubRefreshToken = hubTokenData.refreshToken;
        ctx.session.hubTokenExpiry = Date.now() + hubTokenData.expiresIn * 1000;

        authRoutesDebug(`User and tokens stored in session`, {
          sessionUser: ctx.session.user?.email,
          hasHubAccessToken: !!ctx.session.hubAccessToken,
          sessionKeys: Object.keys(ctx.session),
        });
      } else {
        authRoutesDebug(`WARNING: No session available to store user!`);
      }

      // Also set user in state for immediate use
      ctx.state.user = user;

      // Redirect to original destination or home
      const returnTo = ctx.session?.returnTo || '/';
      if (ctx.session) {
        delete ctx.session.returnTo;
      }

      authRoutesDebug(`Redirecting after successful login`, {
        returnTo,
        sessionUserAfterSet: ctx.session?.user?.email,
      });

      ctx.redirect(returnTo);
    } catch (err) {
      console.error('OAuth callback error:', err);
      authRoutesDebug(`OAuth callback error`, {
        error: err instanceof Error ? err.message : String(err),
      });
      const message = err instanceof Error ? err.message : 'Authentication failed';
      ctx.redirect(`/auth/error?message=${encodeURIComponent(message)}`);
    }
  });

  /**
   * Logout handler - clears session and redirects to login
   */
  async function handleLogout(ctx: Context): Promise<void> {
    // Clear session
    if (ctx.session) {
      ctx.session = null;
    }

    // Clear user from state
    ctx.state.user = undefined;

    // For AJAX requests, return JSON
    if (ctx.accepts('json') && ctx.method === 'POST') {
      ctx.body = { success: true };
      return;
    }

    // For browser requests, redirect to login
    ctx.redirect('/login');
  }

  /**
   * POST /auth/logout
   * Clear session and log out (for AJAX)
   */
  router.post('/logout', handleLogout);

  /**
   * GET /auth/logout
   * Clear session and log out (for browser navigation)
   */
  router.get('/logout', handleLogout);

  /**
   * GET /auth/me
   * Get current user info
   */
  router.get('/me', async (ctx: Context) => {
    const user = ctx.state.user || ctx.session?.user;

    if (!user) {
      ctx.status = 401;
      ctx.body = { error: 'Not authenticated' };
      return;
    }

    ctx.body = { user };
  });

  /**
   * GET /auth/debug
   * Debug endpoint showing current auth state (only available when debug mode is enabled)
   */
  router.get('/debug', async (ctx: Context) => {
    // Only allow in debug mode
    if (!config.debug) {
      ctx.status = 404;
      ctx.body = { error: 'Not found' };
      return;
    }

    const cookieHeader = ctx.headers.cookie || '';
    const cookies = cookieHeader.split(';').map((c) => {
      const [name, ...rest] = c.trim().split('=');
      return {
        name,
        valueLength: rest.join('=').length,
        hasValue: rest.length > 0,
      };
    });

    ctx.body = {
      debug: true,
      timestamp: new Date().toISOString(),
      auth: {
        stateUser: ctx.state.user
          ? {
              id: ctx.state.user.id,
              email: ctx.state.user.email,
              name: ctx.state.user.name,
            }
          : null,
        sessionUser: ctx.session?.user
          ? {
              id: ctx.session.user.id,
              email: ctx.session.user.email,
              name: ctx.session.user.name,
            }
          : null,
        devToken: ctx.state.devToken ? 'present' : 'absent',
        devAuthEnabled: ctx.state.devAuthEnabled || false,
      },
      session: {
        exists: !!ctx.session,
        isNew: ctx.session?.isNew,
        keys: ctx.session ? Object.keys(ctx.session) : [],
        hasUser: !!ctx.session?.user,
        hasReturnTo: !!ctx.session?.returnTo,
        hasOauthState: !!ctx.session?.oauthState,
        hasHubAccessToken: !!ctx.session?.hubAccessToken,
        hasHubRefreshToken: !!ctx.session?.hubRefreshToken,
        hubTokenExpiresIn: ctx.session?.hubTokenExpiry
          ? Math.round((ctx.session.hubTokenExpiry - Date.now()) / 1000) + 's'
          : 'n/a',
      },
      cookies: {
        header: cookieHeader ? 'present' : 'missing',
        count: cookies.length,
        names: cookies.map((c) => c.name).filter(Boolean),
        hasSessionCookie: cookieHeader.includes('scion_sess'),
      },
      config: {
        production: config.production,
        debug: config.debug,
        baseUrl: config.baseUrl,
        hubApiUrl: config.hubApiUrl,
        hasGoogleOAuth: !!config.auth.googleClientId,
        hasGitHubOAuth: !!config.auth.githubClientId,
        authorizedDomains: config.auth.authorizedDomains,
      },
    };
  });

  /**
   * GET /auth/error
   * Display authentication error page
   */
  router.get('/error', async (ctx: Context) => {
    const message = (ctx.query.message as string) || 'Authentication failed';

    // Redirect to login page with error
    ctx.redirect(`/login?error=${encodeURIComponent(message)}`);
  });

  return router;
}
