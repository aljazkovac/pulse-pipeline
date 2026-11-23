import { describe, expect, it, vi } from 'vitest';
import { API_BASE_PATH, createApiClient, type Problem } from './client.ts';

const PROBLEM_BASE = 'https://github.com/aljazkovac/pulse-pipeline/blob/main/docs/api/problems.md#';

function respond(status: number, contentType: string, body: unknown) {
  return vi.fn<typeof fetch>(() =>
    Promise.resolve(
      new Response(JSON.stringify(body), { status, headers: { 'Content-Type': contentType } }),
    ),
  );
}

function requestOf(fetchMock: ReturnType<typeof respond>): Request {
  const [input] = fetchMock.mock.calls[0] ?? [];
  if (!(input instanceof Request)) {
    throw new Error('expected openapi-fetch to pass a Request');
  }
  return input;
}

describe('createApiClient', () => {
  it('calls operations under /api/v1 on the page origin', async () => {
    const fetchMock = respond(200, 'application/json', { status: 'ok' });
    const api = createApiClient({ fetch: fetchMock });

    const { data, error } = await api.GET('/health');

    expect(error).toBeUndefined();
    expect(data?.status).toBe('ok');
    const req = requestOf(fetchMock);
    expect(req.method).toBe('GET');
    expect(req.url).toBe(`${window.location.origin}${API_BASE_PATH}/health`);
  });

  it('honours an explicit origin', async () => {
    const fetchMock = respond(200, 'application/json', { status: 'ok' });
    await createApiClient({ origin: 'http://localhost:5173', fetch: fetchMock }).GET('/health');

    expect(requestOf(fetchMock).url).toBe('http://localhost:5173/api/v1/health');
  });

  it('returns problem details as a typed error', async () => {
    const problem: Problem = {
      type: `${PROBLEM_BASE}unavailable`,
      title: 'Service unavailable',
      status: 503,
      detail: 'The database is unreachable.',
    };
    const api = createApiClient({ fetch: respond(503, 'application/problem+json', problem) });

    const { data, error, response } = await api.GET('/health');

    expect(data).toBeUndefined();
    expect(response.status).toBe(503);
    expect(error).toEqual(problem);
  });
});
