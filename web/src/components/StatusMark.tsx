// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from 'react'
import type { Mark, Tone } from '../status'

// The symbols of design-tokens-v0.1.md "Status marks". A mark is never
// alone: StatusLine always puts the words next to it, so colour is never
// the only signal. The SVG is decorative (aria-hidden); the words carry
// the meaning.
export type MarkSymbol = Mark | 'succeeded'

function shape(s: MarkSymbol) {
  switch (s) {
    case 'online':
      return <circle cx="8" cy="8" r="6" fill="currentColor" />
    case 'running':
      return (
        <>
          <circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M8 2.5 A5.5 5.5 0 0 1 8 13.5 Z" fill="currentColor" />
        </>
      )
    case 'transition':
      return <circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" strokeDasharray="2.6 2.1" />
    case 'stopped':
      return <circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
    case 'stopped-unexpectedly':
      return (
        <>
          <circle cx="8" cy="8" r="5.5" fill="none" stroke="currentColor" strokeWidth="1.5" />
          <path d="M4.5 8 H11.5" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
        </>
      )
    case 'unknown':
      return (
        <>
          <path d="M5.6 5.8 A2.5 2.5 0 1 1 8.9 8.2 C8.2 8.5 8 9 8 9.8" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" />
          <circle cx="8" cy="12.6" r="1" fill="currentColor" />
        </>
      )
    case 'failed':
      return <path d="M4 4 L12 12 M12 4 L4 12" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    case 'succeeded':
      return <path d="M3.5 8.5 L6.8 11.5 L12.5 4.8" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
  }
}

export function StatusMark({ symbol, tone }: { symbol: MarkSymbol; tone: Tone }) {
  return (
    <svg className={`mark tone-${tone}`} data-mark={symbol} width="16" height="16" viewBox="0 0 16 16" aria-hidden="true" focusable="false">
      {shape(symbol)}
    </svg>
  )
}

// StatusLine is a mark and its words, the only way status is shown.
export function StatusLine({ symbol, tone, children }: { symbol: MarkSymbol; tone: Tone; children: ReactNode }) {
  return (
    <span className={`status tone-${tone}`}>
      <StatusMark symbol={symbol} tone={tone} />
      <span className="status-words">{children}</span>
    </span>
  )
}
