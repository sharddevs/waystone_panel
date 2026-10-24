// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from 'react'
import { Link, useLocation } from 'react-router'
import { UserMenu } from './UserMenu'

// Frame is every screen after login (wireframes §0): the top bar with
// the one nav item of v0.1 and the user menu, then the page.
export function Frame({ children }: { children: ReactNode }) {
  // Servers stays the current section on a server's page and on an
  // operation's page: both are reached from it.
  const { pathname } = useLocation()
  const inServers = pathname === '/' || pathname.startsWith('/servers/') || pathname.startsWith('/operations/')
  return (
    <>
      <a className="skip-link" href="#main">
        Skip to content
      </a>
      <header className="topbar">
        <Link to="/" className="brand">
          WAYSTONE
        </Link>
        <nav aria-label="Main">
          <Link to="/" className={inServers ? 'nav-link active' : 'nav-link'} aria-current={inServers ? 'page' : undefined}>
            Servers
          </Link>
        </nav>
        <UserMenu />
      </header>
      <main id="main" className="page" tabIndex={-1}>
        {children}
      </main>
    </>
  )
}
