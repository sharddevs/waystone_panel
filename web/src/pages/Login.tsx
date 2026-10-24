// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useId, useState, type FormEvent } from 'react'
import { Navigate, useLocation, useNavigate, useSearchParams } from 'react-router'
import { ApiError } from '../api'
import { safeNext } from '../safeNext'
import { useSession } from '../session'

// retryText is "Try again in 2 minutes." for a Retry-After in seconds.
export function retryText(seconds: number | undefined): string {
  if (seconds === undefined) return 'Try again later.'
  if (seconds < 60) return seconds <= 1 ? 'Try again in 1 second.' : `Try again in ${seconds} seconds.`
  const m = Math.ceil(seconds / 60)
  return m === 1 ? 'Try again in 1 minute.' : `Try again in ${m} minutes.`
}

type Problem = { kind: 'wrong' } | { kind: 'limited'; until: number; retryAfter: number | undefined } | { kind: 'unavailable' }

// The login page (wireframes §1). A refusal never says whether the
// username or the password was wrong.
export function Login() {
  const session = useSession()
  const navigate = useNavigate()
  const location = useLocation()
  const [params] = useSearchParams()
  const next = safeNext(params.get('next'))
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [problem, setProblem] = useState<Problem | null>(null)
  const [now, setNow] = useState(() => Date.now())
  const userId = useId()
  const passId = useId()

  const limited = problem?.kind === 'limited' && now < problem.until
  useEffect(() => {
    if (problem?.kind !== 'limited') return
    const t = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(t)
  }, [problem])

  if (session.state.status === 'signed-in') return <Navigate to={next} replace />

  const reason = session.state.status === 'signed-out' ? session.state.reason : undefined
  const fromState = (location.state as { reason?: string } | null)?.reason

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (busy || limited) return
    setBusy(true)
    setProblem(null)
    try {
      await session.client.signIn(username, password)
      setPassword('')
      await session.refresh()
      navigate(next, { replace: true })
    } catch (err) {
      setPassword('')
      if (err instanceof ApiError && err.status === 429) {
        setNow(Date.now())
        setProblem({ kind: 'limited', until: Date.now() + (err.retryAfter ?? 60) * 1000, retryAfter: err.retryAfter })
      } else if (err instanceof ApiError && err.status === 401) {
        setProblem({ kind: 'wrong' })
      } else {
        setProblem({ kind: 'unavailable' })
      }
    } finally {
      setBusy(false)
    }
  }

  const note =
    reason === 'expired' || fromState === 'expired'
      ? 'Your session ended. Sign in again.'
      : reason === 'signed-out' || fromState === 'signed-out'
        ? "You've been signed out."
        : undefined

  return (
    <main className="login" id="main">
      <div className="login-card">
        <h1 className="brand-big">WAYSTONE</h1>
        <p className="tagline">Your infrastructure. One control point.</p>
        {note && <p className="note" role="status">{note}</p>}
        <form onSubmit={submit} noValidate={false}>
          <div className="field">
            <label htmlFor={userId}>Username</label>
            <input id={userId} name="username" autoComplete="username" autoCapitalize="none" spellCheck={false} required value={username} onChange={(e) => setUsername(e.target.value)} />
          </div>
          <div className="field">
            <label htmlFor={passId}>Password</label>
            <input id={passId} name="password" type="password" autoComplete="current-password" required value={password} onChange={(e) => setPassword(e.target.value)} />
          </div>
          <button type="submit" className="button primary wide" disabled={busy || limited}>
            {busy ? 'Signing in…' : 'Sign in'}
          </button>
        </form>
        <div aria-live="assertive" className="login-problem">
          {problem?.kind === 'wrong' && <p className="problem">Username or password is wrong.</p>}
          {problem?.kind === 'limited' && limited && (
            <p className="problem">Too many attempts. {retryText(Math.max(1, Math.ceil((problem.until - now) / 1000)))}</p>
          )}
          {problem?.kind === 'unavailable' && <p className="problem">Waystone couldn't check your sign-in. Try again.</p>}
        </div>
      </div>
    </main>
  )
}
