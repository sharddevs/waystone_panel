// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from 'vitest'
import { ApiError, Client, parseRetryAfter } from './api'
import { fakeClient, goldenSession, webError } from './test/fakeApi'

describe('Client', () => {
  it('sends same-origin requests with no caching and no redirects', async () => {
    const { client, calls } = fakeClient(() => ({ status: 200, body: goldenSession }))
    await client.session()
    expect(calls[0]?.method).toBe('GET')
    expect(calls[0]?.path).toBe('/api/v1/session')
    expect(calls[0]?.init).toMatchObject({ credentials: 'same-origin', cache: 'no-store', redirect: 'error' })
    expect(calls[0]?.headers['X-CSRF-Token']).toBeUndefined()
  })

  it('sends the CSRF token and Idempotency-Key on an action', async () => {
    const op = JSON.parse((await import('node:fs')).readFileSync('../internal/view/testdata/operation.json', 'utf8'))
    const { client, calls } = fakeClient(() => ({ status: 202, body: op }))
    await client.act('a/b', 'restart', 'tok', 'key-1')
    expect(calls[0]).toMatchObject({
      method: 'POST',
      path: '/api/v1/servers/a%2Fb/restart',
      headers: { 'X-CSRF-Token': 'tok', 'Idempotency-Key': 'key-1', 'Content-Type': 'application/json' },
      body: {},
    })
  })

  it('sends sign-in without a CSRF token and returns nothing on 204', async () => {
    const { client, calls } = fakeClient(() => ({ status: 204 }))
    await expect(client.signIn('trevor', 'pw')).resolves.toBeUndefined()
    expect(calls[0]).toMatchObject({ method: 'POST', path: '/api/v1/session', body: { username: 'trevor', password: 'pw' } })
    expect(calls[0]?.headers['X-CSRF-Token']).toBeUndefined()
  })

  it('builds the operations query', async () => {
    const { client, calls } = fakeClient(() => ({ status: 200, body: { operations: [] } }))
    await client.operations({ serverId: 's1', limit: 10 })
    await client.operations()
    expect(calls.map((c) => c.path)).toEqual(['/api/v1/operations?server_id=s1&limit=10', '/api/v1/operations'])
  })

  it('turns an error body into an ApiError', async () => {
    const { client } = fakeClient(() => ({
      status: 409,
      body: { error: { code: 'operation_in_progress', message: 'Busy.', request_id: 'r1', operation_id: 'o1' } },
    }))
    const err = await client.server('s').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 409, code: 'operation_in_progress', message: 'Busy.', requestId: 'r1', operationId: 'o1' })
  })

  it('reads Retry-After on a 429', async () => {
    const { client } = fakeClient(() => ({ ...webError('too_many_requests', 429), headers: { 'Retry-After': '120' } }))
    await expect(client.signIn('a', 'b')).rejects.toMatchObject({ status: 429, code: 'too_many_requests', retryAfter: 120 })
  })

  it('a body that is not an error body is bad_response', async () => {
    const { client } = fakeClient(() => ({ status: 502, body: 'Bad Gateway' }))
    await expect(client.servers()).rejects.toMatchObject({ status: 502, code: 'bad_response' })
  })

  it('a success body of the wrong shape is bad_response', async () => {
    const { client } = fakeClient(() => ({ status: 200, body: { servers: [{ id: 1 }] } }))
    await expect(client.servers()).rejects.toMatchObject({ status: 200, code: 'bad_response' })
  })

  it('a failed fetch is network', async () => {
    const client = new Client(() => Promise.reject(new TypeError('offline')))
    await expect(client.servers()).rejects.toMatchObject({ status: 0, code: 'network' })
  })
})

it('parseRetryAfter only takes seconds', () => {
  expect(parseRetryAfter('60')).toBe(60)
  expect(parseRetryAfter(null)).toBeUndefined()
  expect(parseRetryAfter('Wed, 21 Oct 2015 07:28:00 GMT')).toBeUndefined()
  expect(parseRetryAfter('-1')).toBeUndefined()
})
