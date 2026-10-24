// SPDX-License-Identifier: AGPL-3.0-only

import type { ReactNode } from 'react'
import { Link, NavLink } from 'react-router'
import { UserMenu } from './UserMenu'

// Frame is every screen after login (wireframes §0): the top bar with
// the one nav item of v0.1 and the user menu, then the page.
export function Frame({ children }: { children: ReactNode }) {
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
          <NavLink to="/" end className="nav-link">
            Servers
          </NavLink>
        </nav>
        <UserMenu />
      </header>
      <main id="main" className="page" tabIndex={-1}>
        {children}
      </main>
    </>
  )
}
