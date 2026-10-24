// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'
import {
  decodeCreatedServer,
  decodeEnrollmentList,
  decodeErrorBody,
  decodeNewEnrollment,
  decodeNode,
  decodeNodeList,
  decodeOperation,
  decodeOperationList,
  decodeServer,
  decodeServerList,
  decodeSession,
} from './api'
import type { Decoder } from './decode'

// Q9: the Go tests write these files from the real types (go test
// ./internal/view ./internal/service ./internal/web ./internal/session
// -run Golden -update). Decoding one and getting exactly the same value
// back means every field the Go side sends is one the client knows,
// with the type it expects, and nothing the client needs is missing.
const files: [string, Decoder<unknown>][] = [
  ['internal/view/testdata/server.json', decodeServer],
  ['internal/view/testdata/servers.json', decodeServerList],
  ['internal/view/testdata/operation.json', decodeOperation],
  ['internal/view/testdata/operations.json', decodeOperationList],
  ['internal/view/testdata/node.json', decodeNode],
  ['internal/view/testdata/nodes.json', decodeNodeList],
  ['internal/view/testdata/enrollments.json', decodeEnrollmentList],
  ['internal/view/testdata/enrollment_new.json', decodeNewEnrollment],
  ['internal/view/testdata/created.json', decodeCreatedServer],
  ['internal/service/testdata/error.json', decodeErrorBody],
  ['internal/web/testdata/error.json', decodeErrorBody],
  ['internal/session/testdata/session.json', decodeSession],
]

describe.each(files)('%s', (file, decode) => {
  const json: unknown = JSON.parse(readFileSync(`../${file}`, 'utf8'))

  it('decodes to exactly the same value', () => {
    expect(decode(json, 'golden')).toStrictEqual(json)
  })
})

it('a field the client needs going missing fails', () => {
  const s = JSON.parse(readFileSync('../internal/view/testdata/server.json', 'utf8')) as Record<string, unknown>
  delete s.desired_state
  expect(() => decodeServer(s, 'server')).toThrow(/desired_state/)
})

it('a field the client does not know shows up as a difference', () => {
  const s = JSON.parse(readFileSync('../internal/view/testdata/server.json', 'utf8')) as Record<string, unknown>
  s.tenant_id = 'x'
  expect(decodeServer(s, 'server')).not.toStrictEqual(s)
})

it('a state the client does not know fails', () => {
  const s = JSON.parse(readFileSync('../internal/view/testdata/server.json', 'utf8')) as { process: { state: string } }
  s.process.state = 'paused'
  expect(() => decodeServer(s, 'server')).toThrow(/process\.state/)
})
