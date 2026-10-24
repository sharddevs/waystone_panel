// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from 'react'
import { Navigate, Route, Routes, useLocation } from 'react-router'
import { Frame } from './components/Frame'
import { CreateServer } from './pages/CreateServer'
import { Login } from './pages/Login'
import { NodesPage } from './pages/NodesPage'
import { NotFound } from './pages/NotFound'
import { OperationPageRoute } from './pages/OperationPage'
import { ServerList } from './pages/ServerList'
import { ServerPageRoute } from './pages/ServerPage'
import { useSession } from './session'

// RequireSession shows its page only to a signed-in user. Anyone else
// goes to the login page, which comes back here afterwards (C-W8).
function RequireSession({ children }: { children: ReactNode }) {
  const { state, refresh } = useSession()
  const location = useLocation()
  switch (state.status) {
    case 'loading':
      return <p className="loading" role="status">Loading…</p>
    case 'error':
      return (
        <main className="page" id="main">
          <p className="problem">Couldn't reach Waystone.</p>
          <button type="button" className="button" onClick={() => void refresh()}>
            Try again
          </button>
        </main>
      )
    case 'signed-out': {
      const next = location.pathname + location.search
      const to = next === '/' ? '/login' : `/login?next=${encodeURIComponent(next)}`
      return <Navigate to={to} replace state={{ reason: state.reason }} />
    }
    case 'signed-in':
      return <Frame>{children}</Frame>
  }
}

export function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route path="/" element={<RequireSession><ServerList /></RequireSession>} />
      <Route path="/servers/new" element={<RequireSession><CreateServer /></RequireSession>} />
      <Route path="/servers/:id" element={<RequireSession><ServerPageRoute /></RequireSession>} />
      <Route path="/operations/:id" element={<RequireSession><OperationPageRoute /></RequireSession>} />
      <Route path="/nodes" element={<RequireSession><NodesPage /></RequireSession>} />
      <Route path="*" element={<RequireSession><NotFound /></RequireSession>} />
    </Routes>
  )
}
