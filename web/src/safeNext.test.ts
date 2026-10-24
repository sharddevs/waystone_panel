// SPDX-License-Identifier: AGPL-3.0-only

import { expect, it } from 'vitest'
import { safeNext } from './safeNext'

// The same cases as internal/web/redirect_test.go TestSafeNext (C-W8).
it('keeps same-origin paths', () => {
  for (const p of ['/', '/servers', '/servers/0199e1a0-0000-7000-8000-0000000000a1', '/a?b=c', '/a#frag', '/%E2%9C%93', '/a/../b', '/a%2Fb']) {
    expect(safeNext(p), p).toBe(p)
  }
})

it('refuses everything else', () => {
  for (const p of [
    '', 'servers', '//evil.test', '///evil.test', '/\\evil.test', '\\\\evil.test', '/a\\b',
    'https://evil.test', 'http://evil.test/', 'javascript:alert(1)', 'data:text/html,x',
    '%2F%2Fevil.test', '/%2Fevil.test', '/%2F%2Fevil.test', '/%5Cevil.test', '/%5cevil.test',
    '/a b', '/a\tb', '/a\nb', '/a\rb', '/\x00', '/%00', '/%0d%0aSet-Cookie:x', '/%7f',
    '/' + 'a'.repeat(2048), '/%zz', ' /servers', '/servers\x7f',
  ]) {
    expect(safeNext(p), JSON.stringify(p)).toBe('/')
  }
  expect(safeNext(null)).toBe('/')
  expect(safeNext(undefined)).toBe('/')
})

it('never goes back to the login page', () => {
  expect(safeNext('/login')).toBe('/')
  expect(safeNext('/login?next=%2Fservers')).toBe('/')
})
