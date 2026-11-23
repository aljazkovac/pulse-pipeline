/**
 * Where the Vite dev and preview servers forward `/api` requests.
 *
 * The API and the web app share an origin (ADR-0005): in development the browser only
 * talks to Vite on :5173, which proxies `/api` to the Go API. `PULSE_API_ORIGIN` wins;
 * otherwise the API's own `API_PORT` (from the repository `.env`) on localhost is used.
 */
export const DEFAULT_API_PORT = 8080;

export type ProxyEnv = Readonly<Record<string, string | undefined>>;

export function apiProxyTarget(env: ProxyEnv): string {
  const explicit = env.PULSE_API_ORIGIN?.trim();
  if (explicit) {
    return parseOrigin(explicit);
  }

  const rawPort = env.API_PORT?.trim();
  if (!rawPort) {
    return `http://localhost:${String(DEFAULT_API_PORT)}`;
  }
  const port = Number(rawPort);
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error(`API_PORT must be an integer between 1 and 65535, got "${rawPort}"`);
  }
  return `http://localhost:${String(port)}`;
}

function parseOrigin(value: string): string {
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    throw new Error(`PULSE_API_ORIGIN must be an absolute http(s) URL, got "${value}"`);
  }
  if (url.protocol !== 'http:' && url.protocol !== 'https:') {
    throw new Error(`PULSE_API_ORIGIN must use http or https, got "${url.protocol}"`);
  }
  if (url.pathname !== '/' || url.search !== '' || url.hash !== '') {
    throw new Error(`PULSE_API_ORIGIN must be an origin without a path, got "${value}"`);
  }
  return url.origin;
}
