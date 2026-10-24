// SPDX-License-Identifier: AGPL-3.0-only

// safeNext is web.SafeNext (internal/web/redirect.go, C-W8) for the
// client: after sign-in the page only goes to a same-origin path. It
// accepts only a path starting with a single "/", never "//" or "/\", a
// backslash, a control character or space, or anything that becomes one
// of those after percent-decoding. Anything else is "/".
export function safeNext(next: string | null | undefined): string {
  if (!next || next.length > 2048 || next[0] !== '/') return '/'
  if (next.startsWith('//') || next.includes('\\')) return '/'
  for (let i = 0; i < next.length; i++) {
    const c = next.charCodeAt(i)
    if (c <= 0x20 || c === 0x7f) return '/'
  }
  let decoded: string
  try {
    decoded = decodeURIComponent(next.split(/[?#]/, 1)[0] ?? '')
  } catch {
    return '/'
  }
  if (decoded.startsWith('//') || decoded.includes('\\')) return '/'
  for (let i = 0; i < decoded.length; i++) {
    const c = decoded.charCodeAt(i)
    if (c < 0x20 || c === 0x7f) return '/'
  }
  // The login page itself is never a destination.
  if (decoded === '/login' || decoded.startsWith('/login?')) return '/'
  return next
}
