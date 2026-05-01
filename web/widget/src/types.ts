// Wire types — must match internal/apiserver/index.go (TupleEntry) and
// internal/controller/translator_render.go (cfg map).

export interface TupleEntry {
  service: string
  alternative: string
  tuple: string
  routingKey: string
  sourceCookie?: string
}

export interface TupleListResponse {
  items: TupleEntry[]
  nextCursor?: string
}

export interface WidgetMargin {
  top?: string
  right?: string
  bottom?: string
  left?: string
}

export interface WidgetHotkey {
  open?: string
}

// `anchor` decides which viewport edge/corner the widget docks to. The
// allowed values depend on `mode`:
//   - float:   one of the four corners
//   - sidebar: one of the two vertical sides
// Defaults are 'bottom-right' (float) and 'right' (sidebar).
export type FloatAnchor = 'top-left' | 'top-right' | 'bottom-left' | 'bottom-right'
export type SidebarAnchor = 'left' | 'right'

export interface WidgetStyle {
  mode?: 'float' | 'sidebar'
  anchor?: FloatAnchor | SidebarAnchor
  margin?: WidgetMargin
  hotkey?: WidgetHotkey
}

// Browser-side overrides installed at boot. When fetch / XHR are toggled
// on the widget monkey-patches the global so every outbound request
// carries the W3C Baggage header populated from the active source
// cookie — that's what makes routing work when the page calls a
// *different origin* (cookies are dropped on cross-origin requests,
// but Baggage rides through unchanged, and the translator already
// reads Baggage as its inter-tier wire format).
export interface WidgetJSOverride {
  enable?: boolean
}

export interface WidgetJS {
  fetch?: WidgetJSOverride
  xmlhttprequest?: WidgetJSOverride
}

export interface WidgetConfig {
  target: string
  namespace: string
  routingKey: string
  sourceCookie: string
  pathPrefix: string
  style?: WidgetStyle
  js?: WidgetJS
  // Set by the operator's /widget/preview handler so the widget knows to
  // call same-origin /api/v1/* instead of the prefixed proxy path.
  previewMode?: boolean
}
