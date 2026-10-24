// SPDX-License-Identifier: AGPL-3.0-only

import { readFileSync } from 'node:fs'
import { describe, expect, it } from 'vitest'

// Vitest runs in web/.
const css = readFileSync('src/tokens.css', 'utf8')

// WCAG AA (4.5:1) over every token set in tokens.css: each theme, in
// light and in dark (design-tokens-v0.1.md "Contrast"). System mode uses
// the same two sets. Density changes no colour.

type Colours = Record<string, { light: string; dark: string }>

function themeBlock(theme: string): Colours {
  const re = new RegExp(`:root\\[data-theme='${theme}'\\]\\s*\\{([^}]*)\\}`)
  const m = re.exec(css)
  if (!m?.[1]) throw new Error(`no block for ${theme}`)
  const out: Colours = {}
  for (const d of m[1].matchAll(/--([a-z0-9-]+):\s*light-dark\(\s*(#[0-9a-f]{6})\s*,\s*(#[0-9a-f]{6})\s*\)/g)) {
    const [, name, light, dark] = d
    if (name && light && dark) out[name] = { light, dark }
  }
  return out
}

function luminance(hex: string): number {
  const c = [1, 3, 5].map((i) => parseInt(hex.slice(i, i + 2), 16) / 255)
  const [r, g, b] = c.map((v) => (v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4)) as [number, number, number]
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

function contrast(a: string, b: string): number {
  const [x, y] = [luminance(a), luminance(b)]
  return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05)
}

const tokens = ['bg', 'surface', 'surface-2', 'border', 'fg', 'fg-2', 'fg-3', 'accent', 'accent-fg', 'accent-soft', 'ok', 'warn', 'bad', 'unk']

// Text and status colours on the backgrounds they are used on.
const text = ['fg', 'fg-2', 'fg-3', 'accent', 'ok', 'warn', 'bad', 'unk']
const pairs: [string, string][] = [
  ...text.flatMap((t): [string, string][] => [
    [t, 'bg'],
    [t, 'surface'],
  ]),
  ['fg', 'surface-2'],
  ['fg-2', 'surface-2'],
  // accent text on accent-soft is 4.44 in light Stone: not allowed, so
  // the active nav item uses fg on accent-soft.
  ['fg', 'accent-soft'],
  ['accent-fg', 'accent'],
]

describe.each(['stone', 'slate'])('%s', (theme) => {
  const colours = themeBlock(theme)

  it('defines every token for both modes', () => {
    expect(Object.keys(colours).sort()).toEqual([...tokens].sort())
  })

  describe.each(['light', 'dark'] as const)('%s', (mode) => {
    it.each(pairs)('%s on %s passes AA', (fg, bg) => {
      const [a, b] = [colours[fg], colours[bg]]
      if (!a || !b) throw new Error(`missing ${fg} or ${bg}`)
      const ratio = contrast(a[mode], b[mode])
      expect(ratio, `${fg} on ${bg}: ${ratio.toFixed(2)}`).toBeGreaterThanOrEqual(4.5)
    })
  })
})

it('stone is also the default (:root)', () => {
  expect(css).toMatch(/:root,\s*:root\[data-theme='stone'\]\s*\{/)
})

it('the formula matches known values', () => {
  expect(contrast('#000000', '#ffffff')).toBeCloseTo(21, 5)
  expect(contrast('#8a8276', '#1c1b19')).toBeCloseTo(4.54, 2) // design-tokens: tightest stone pair
})
