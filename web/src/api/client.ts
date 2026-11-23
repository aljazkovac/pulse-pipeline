import createClient, { type Client } from 'openapi-fetch';
import type { components, paths } from './schema';

/**
 * Typed client for the /api/v1 contract (backend/api/openapi.yaml, ADR-0005 §1).
 * Request and response types come from the generated ./schema.d.ts, so a contract
 * change that breaks the frontend fails `npm run typecheck`. Run
 * `npm run generate:api` (or `make generate`) after editing the contract.
 */
export type ApiClient = Client<paths>;

/** RFC 9457 problem details; every API error response has this shape. */
export type Problem = components['schemas']['Problem'];

/** The OpenAPI server URL: every operation path is relative to it. */
export const API_BASE_PATH = '/api/v1';

export interface ApiClientOptions {
  /** Origin the API is served from. Defaults to the page's own origin. */
  origin?: string;
  /** Fetch implementation, for tests. */
  fetch?: typeof globalThis.fetch;
}

/**
 * The API shares the web app's origin: in development the Vite server proxies
 * /api to the Go API, in production the ingress does (ADR-0005). The session
 * cookie therefore travels with the default same-origin credentials.
 */
export function createApiClient(options: ApiClientOptions = {}): ApiClient {
  const origin = options.origin ?? globalThis.location.origin;
  return createClient<paths>({
    baseUrl: `${origin}${API_BASE_PATH}`,
    ...(options.fetch ? { fetch: options.fetch } : {}),
  });
}
