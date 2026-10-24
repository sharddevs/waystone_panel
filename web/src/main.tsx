// SPDX-License-Identifier: AGPL-3.0-only

// First, before React renders anything: the cached appearance settings,
// so the page never flashes the wrong theme (scope §6; the CSP forbids an
// inline script in index.html).
import { applyCached } from './appearance'
applyCached()

import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router'
import './tokens.css'
import './fonts.css'
import './base.css'
import { App } from './App'
import { SessionProvider } from './session'

const root = document.getElementById('root')
if (root === null) throw new Error('no #root')
createRoot(root).render(
  <StrictMode>
    <BrowserRouter>
      <SessionProvider>
        <App />
      </SessionProvider>
    </BrowserRouter>
  </StrictMode>,
)
