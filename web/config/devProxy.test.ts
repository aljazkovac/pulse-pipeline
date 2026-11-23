// @vitest-environment node
import { describe, expect, it } from 'vitest';
import { apiProxyTarget } from './devProxy.ts';

describe('apiProxyTarget', () => {
  it.each([
    { name: 'defaults to the API on localhost:8080', env: {}, want: 'http://localhost:8080' },
    { name: 'follows API_PORT', env: { API_PORT: '9000' }, want: 'http://localhost:9000' },
    { name: 'ignores a blank API_PORT', env: { API_PORT: '  ' }, want: 'http://localhost:8080' },
    {
      name: 'prefers PULSE_API_ORIGIN over API_PORT',
      env: { PULSE_API_ORIGIN: 'http://127.0.0.1:18080', API_PORT: '9000' },
      want: 'http://127.0.0.1:18080',
    },
    {
      name: 'normalises a trailing slash',
      env: { PULSE_API_ORIGIN: 'https://api.example.test/' },
      want: 'https://api.example.test',
    },
  ])('$name', ({ env, want }) => {
    expect(apiProxyTarget(env)).toBe(want);
  });

  it.each([
    { name: 'a non-numeric port', env: { API_PORT: 'eighty' }, error: /API_PORT/ },
    { name: 'an out-of-range port', env: { API_PORT: '70000' }, error: /API_PORT/ },
    { name: 'a relative origin', env: { PULSE_API_ORIGIN: 'localhost:8080' }, error: /http/ },
    { name: 'a non-http origin', env: { PULSE_API_ORIGIN: 'ftp://localhost' }, error: /http or https/ },
    {
      name: 'an origin with a path',
      env: { PULSE_API_ORIGIN: 'http://localhost:8080/api' },
      error: /without a path/,
    },
  ])('rejects $name', ({ env, error }) => {
    expect(() => apiProxyTarget(env)).toThrow(error);
  });
});
