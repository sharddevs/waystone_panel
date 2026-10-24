// SPDX-License-Identifier: AGPL-3.0-only

// The pages themselves arrive in 5d (scope §7). Until then each route
// says what it will be, so the frame, the session and the routing can be
// tried end to end.
export function Placeholder({ title }: { title: string }) {
  return (
    <>
      <h1 className="page-title">{title}</h1>
      <p className="meta">This page arrives in the next update.</p>
    </>
  )
}

export function NotFound() {
  return (
    <>
      <h1 className="page-title">Page not found</h1>
      <p className="meta">There is nothing at this address.</p>
    </>
  )
}
