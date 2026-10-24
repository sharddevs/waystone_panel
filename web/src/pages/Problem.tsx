// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from 'react'
import type { ApiError } from '../api'
import { errorText } from '../format'

// Problem is a "couldn't load" block: what failed, why, a way to try
// again, and the request ID in small text when there is one (A5).
export function Problem({ error, retry, children }: { error: ApiError; retry?: () => void; children: ReactNode }) {
  return (
    <div className="problem-block" role="alert">
      <p className="problem">
        {children} {errorText(error)}
      </p>
      {retry && (
        <button type="button" className="button" onClick={retry}>
          Try again
        </button>
      )}
      {error.requestId && <p className="meta">Request ID <span className="mono">{error.requestId}</span></p>}
    </div>
  )
}
