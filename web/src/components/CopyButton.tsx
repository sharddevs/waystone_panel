// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from 'react'

// CopyButton puts text on the clipboard and says whether that worked.
// label is its accessible name when "Copy" alone wouldn't say what it
// copies.
export function CopyButton({ text, label }: { text: string; label?: string }) {
  const [done, setDone] = useState<'copied' | 'failed' | null>(null)
  return (
    <>
      <button
        type="button"
        className="button small"
        aria-label={label}
        onClick={() => {
          // No clipboard outside a secure context: say so instead of throwing.
          const write = navigator.clipboard?.writeText(text) ?? Promise.reject(new Error('no clipboard'))
          write.then(
            () => setDone('copied'),
            () => setDone('failed'),
          )
        }}
      >
        Copy
      </button>
      <span className="meta" aria-live="polite">
        {done === 'copied' ? ' Copied.' : done === 'failed' ? " Couldn't copy. Select it and copy it yourself." : ''}
      </span>
    </>
  )
}
