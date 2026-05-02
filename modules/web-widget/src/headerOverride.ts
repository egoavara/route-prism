// Baggage header override: when js.fetch / js.xmlhttprequest is enabled
// the widget monkey-patches window.fetch / XMLHttpRequest at boot so
// every outbound HTTP request carries the active routing context as the
// W3C Baggage header (`<key>=<value>,<key>=<value>`). This is the same
// header route-prism translators already use as the inter-tier wire
// format, so cross-origin pages that lose their cookie still arrive at
// the receiving translator with a routing context it can read out of
// the box.
//
// We deliberately attach Baggage on every call site rather than gating
// by URL — different origins are exactly the case this exists for, so
// an allow-list keyed off the page's origin would defeat the purpose.

import { parse, readCookie } from './cookieStore'
import type { WidgetConfig } from './types'

const HEADER_NAME = 'Baggage'

// readBaggageValue serializes the cookie's entries to W3C Baggage form,
// dropping `.` (no-fork) members so they don't pollute downstream
// matchers. Returns "" when there's nothing to send.
function readBaggageValue(cfg: WidgetConfig): string {
  if (!cfg.sourceCookie) return ''
  const entries = parse(readCookie(cfg.sourceCookie))
  if (entries.size === 0) return ''
  const parts: string[] = []
  for (const [k, v] of entries) {
    if (v === '.' || v === '') continue
    parts.push(`${k}=${v}`)
  }
  return parts.join(',')
}

// Re-entrancy guard. Without it, two widget bundles loaded on the same
// page (HMR, accidentally-double-injected script tags) would chain
// patches and double-set the header.
const PATCHED = '__routePrismPatched'

export function installHeaderOverrides(cfg: WidgetConfig): void {
  const js = cfg.js
  if (!js) return
  const fetchOn = !!js.fetch?.enable
  const xhrOn = !!js.xmlhttprequest?.enable
  if (!fetchOn && !xhrOn) return

  const win = window as unknown as Record<string, unknown>
  if (win[PATCHED]) return
  win[PATCHED] = true

  if (fetchOn && typeof window.fetch === 'function') {
    const origFetch = window.fetch.bind(window)
    window.fetch = (input: RequestInfo | URL, init?: RequestInit) => {
      const value = readBaggageValue(cfg)
      if (!value) return origFetch(input, init)
      const headers = new Headers(
        init?.headers ?? (input instanceof Request ? input.headers : undefined),
      )
      // Only set when absent — explicit caller-supplied Baggage wins so
      // tests / debugging can override.
      if (!headers.has(HEADER_NAME)) headers.set(HEADER_NAME, value)
      const nextInit: RequestInit = { ...(init ?? {}), headers }
      return origFetch(input, nextInit)
    }
  }

  if (xhrOn && typeof XMLHttpRequest !== 'undefined') {
    const proto = XMLHttpRequest.prototype
    const origSend = proto.send
    const origOpen = proto.open
    // Track per-instance state on a hidden field; we set the header in
    // send() because setRequestHeader must be called between open() and
    // send() and we want one consistent attach point.
    const STATE = '__routePrismHeaderApplied'
    proto.open = function (
      this: XMLHttpRequest,
      method: string,
      url: string | URL,
      async?: boolean,
      user?: string | null,
      pass?: string | null,
    ) {
      ;(this as unknown as Record<string, unknown>)[STATE] = false
      if (arguments.length >= 3) {
        return origOpen.call(this, method, url, async ?? true, user ?? null, pass ?? null)
      }
      return origOpen.call(this, method, url, true)
    } as typeof proto.open
    proto.send = function (this: XMLHttpRequest, body?: Document | XMLHttpRequestBodyInit | null) {
      const self = this as XMLHttpRequest & Record<string, unknown>
      if (!self[STATE]) {
        const value = readBaggageValue(cfg)
        if (value) {
          try {
            self.setRequestHeader(HEADER_NAME, value)
          } catch {
            // Some browsers throw if send was called before open finished
            // setting up the request slot. Fall through silently — the
            // request still goes, just without our header.
          }
          self[STATE] = true
        }
      }
      return origSend.call(this, body ?? null)
    } as typeof proto.send
  }
}
