// Multi-tier cookie helpers. Mirrors the parser in
// internal/controller/templates/nginx.smart.conf.tmpl — keep them in sync.
//
// Wire format: "<routingKey>:<variant>|<routingKey>:<variant>|..."
// A variant of "." means "no override at this tier" (equivalent to absent).

export type Entries = Map<string, string>

export function parse(raw: string): Entries {
  const out: Entries = new Map()
  if (!raw) return out
  for (const part of raw.split('|')) {
    const colon = part.indexOf(':')
    if (colon < 0) continue
    const k = part.slice(0, colon)
    const v = part.slice(colon + 1)
    if (k) out.set(k, v)
  }
  return out
}

export function serialize(entries: Entries): string {
  const parts: string[] = []
  for (const [k, v] of entries) parts.push(`${k}:${v}`)
  return parts.join('|')
}

// Cookie-name and cookie-value handling per RFC 6265 §4.1.1:
//   - cookie-name: only token chars (no `=`, no separators). Names we
//     write here ("x-route-prism") are already token-safe, so no encoding.
//   - cookie-value: cookie-octet covers `:` (0x3A) and `|` (0x7C), which
//     are exactly the separators in our `<key>:<val>|<key>:<val>` wire
//     format. URL-encoding the value would produce `%3A`/`%7C` on the
//     wire — the translator's nginx parser then sees a single opaque
//     blob and silently falls back to the default route. So we write the
//     value RAW and only fall back to URL-decoding on read for backwards
//     compatibility with cookies set by older builds.
export function readCookie(name: string): string {
  for (const part of document.cookie.split(';')) {
    const trimmed = part.trim()
    const eq = trimmed.indexOf('=')
    if (eq < 0) continue
    const key = trimmed.slice(0, eq)
    if (key !== name && key !== encodeURIComponent(name)) continue
    const raw = trimmed.slice(eq + 1)
    // If the value contains a percent-escape, decode it; otherwise hand
    // it back verbatim so we don't mangle a value that legitimately
    // includes `%` (rare for our format but safe).
    return raw.includes('%') ? decodeURIComponent(raw) : raw
  }
  return ''
}

export interface WriteOptions {
  domain?: string
  path?: string
  secure?: boolean
  sameSite?: 'Lax' | 'Strict' | 'None'
  maxAgeSeconds?: number
}

export function writeCookie(name: string, value: string, opts: WriteOptions = {}): void {
  const segs = [`${name}=${value}`]
  segs.push(`Path=${opts.path ?? '/'}`)
  if (opts.domain) segs.push(`Domain=${opts.domain}`)
  segs.push(`SameSite=${opts.sameSite ?? 'Lax'}`)
  if (opts.secure ?? location.protocol === 'https:') segs.push('Secure')
  if (opts.maxAgeSeconds != null) segs.push(`Max-Age=${opts.maxAgeSeconds}`)
  document.cookie = segs.join('; ')
}

// Update the SourceCookie's entry for `routingKey` to `variant`, preserving
// every other tier's entry untouched. Returns the new cookie string for
// caller convenience (e.g. logging / tests).
export function setEntry(
  cookieName: string,
  routingKey: string,
  variant: string,
  writeOpts: WriteOptions = {},
): string {
  const entries = parse(readCookie(cookieName))
  entries.set(routingKey, variant)
  const next = serialize(entries)
  writeCookie(cookieName, next, writeOpts)
  return next
}
