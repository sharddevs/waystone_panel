// SPDX-License-Identifier: AGPL-3.0-only

import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { expect, it } from 'vitest'

// The font files are IBM's, unchanged (src/fonts/SOURCE.md).
const pinned: Record<string, string> = {
  'IBMPlexSans-Regular.woff2': 'ba711a3085ff9f27440b6b9c4550cfc47c97bf36591d5da958b975bb3add8c1a',
  'IBMPlexSans-Medium.woff2': '5660f8a658f8bb50dbc005232f885eadffd2bc1c235c4f6fbb63469d1f9cde6d',
  'IBMPlexSans-SemiBold.woff2': 'f78048030eab62e860efa39a0df79e2e5581bf122eb95b9bc42c0b8a4988d205',
  'IBMPlexMono-Regular.woff2': 'ba204497f16b6d334cee9d1e963a831b73e3a56e1d6300a8489d18df7214b350',
  'IBMPlexMono-Medium.woff2': '33faf307fa6031fb4062276d7320a6d632de890cbb347576fd80cfa01077bc25',
  'OFL.txt': '7e6b2818edbd8f6a01ae80641cc8f16a51080d08fb4e532be3a0b6f74adb07da',
}

it.each(Object.entries(pinned))('%s matches its pinned sha256', (file, sum) => {
  expect(createHash('sha256').update(readFileSync(`src/fonts/${file}`)).digest('hex')).toBe(sum)
})

it('the licence also ships in the build, unchanged', () => {
  expect(readFileSync('public/licenses/OFL.txt', 'utf8')).toBe(readFileSync('src/fonts/OFL.txt', 'utf8'))
})

it('fonts.css only uses the bundled files', () => {
  const css = readFileSync('src/fonts.css', 'utf8')
  const urls = [...css.matchAll(/url\(([^)]*)\)/g)].map((m) => m[1])
  expect(urls).toHaveLength(5)
  for (const u of urls) expect(u).toMatch(/^'\.\/fonts\/IBMPlex(Sans|Mono)-[A-Za-z]+\.woff2'$/)
})
