import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks'
import createFuzzySearch from '@nozbe/microfuzz'
import type { TupleEntry, WidgetConfig } from './types'
import { listTuples } from './api'
import { parse, readCookie, setEntry } from './cookieStore'
import { formatChord, useHotkey } from './useHotkey'
import { TupleList, type TupleRow } from './components/TupleList'

interface Props {
  config: WidgetConfig
}

export function App({ config }: Props) {
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [items, setItems] = useState<TupleEntry[]>([])
  const [cursor, setCursor] = useState(0)
  const [entries, setEntries] = useState<Map<string, string>>(() => readEntries(config))

  const inputRef = useRef<HTMLInputElement | null>(null)
  const rootRef = useRef<HTMLDivElement | null>(null)
  const reqRef = useRef(0)
  // Snapshot of own-tier variant at mount. Reload is required only when
  // *this* tier's variant has drifted from what the page was rendered
  // against; cross-tier overrides take effect on the next downstream call
  // and don't need a host-page reload.
  const initialOwnVariantRef = useRef<string>(entries.get(config.routingKey) ?? '')

  const mode = config.style?.mode ?? 'float'
  const anchor = config.style?.anchor ?? (mode === 'float' ? 'bottom-right' : 'right')
  const margin = config.style?.margin ?? {}
  const hostStyle: Record<string, string> = {}
  if (margin.top) hostStyle['--rp-margin-top'] = margin.top
  if (margin.right) hostStyle['--rp-margin-right'] = margin.right
  if (margin.bottom) hostStyle['--rp-margin-bottom'] = margin.bottom
  if (margin.left) hostStyle['--rp-margin-left'] = margin.left

  const ownVariant = entries.get(config.routingKey) ?? ''
  const isOwnActive = ownVariant !== '' && ownVariant !== '.'
  const hasAnyActive = useMemo(() => {
    for (const v of entries.values()) if (v && v !== '.') return true
    return false
  }, [entries])
  const needsReload = ownVariant !== initialOwnVariantRef.current

  // selectedList is every (routingKey, alternative) the user currently
  // overrides — i.e. every entry whose value is not the "." default.
  // The closed-state button cycles through this list so a multi-tier
  // selection (e.g. demo.web=canary AND demo.db=laptop) doesn't get
  // hidden behind a single static label.
  const selectedList = useMemo(() => {
    const out: { routingKey: string; alternative: string }[] = []
    for (const [k, v] of entries) {
      if (v && v !== '.') out.push({ routingKey: k, alternative: v })
    }
    return out
  }, [entries])

  // unavailableList is the subset of selectedList currently failing
  // their reachability check (RemoteRoute upstreams that don't respond
  // — could be a dev PC, a staging server, anything; the wording stays
  // generic). Drives the closed-state alarm so users notice "this
  // selection is broken right now" without opening the panel.
  const [unavailableList, setUnavailableList] = useState<
    { routingKey: string; alternative: string }[]
  >([])
  const hasUnavailable = unavailableList.length > 0

  // Background poll — refreshes the alarm list every 15s independent
  // of whether the panel is open. Cheap (one /tuple call, <1KB on
  // typical demos). Skipped when the user has no active overrides.
  useEffect(() => {
    if (selectedList.length === 0) {
      setUnavailableList([])
      return
    }
    let cancelled = false
    const check = async () => {
      try {
        const resp = await listTuples(config, '', 500)
        if (cancelled) return
        const next: { routingKey: string; alternative: string }[] = []
        for (const e of resp.items ?? []) {
          if (!e.remote || e.reachable !== false) continue
          if (entries.get(e.routingKey) === e.alternative) {
            next.push({ routingKey: e.routingKey, alternative: e.alternative })
          }
        }
        setUnavailableList(next)
      } catch {
        // leave previous state — transient network errors shouldn't
        // flip the alarm in either direction.
      }
    }
    void check()
    const id = window.setInterval(check, 15_000)
    return () => {
      cancelled = true
      window.clearInterval(id)
    }
  }, [entries, selectedList, config])

  // cycleIdx rotates through whichever list the closed-state button
  // is currently surfacing (alarm > active). Reset to 0 whenever the
  // active list changes shape so the rotation always starts from the
  // top after a selection edit. Interval is intentionally slow (5s)
  // so the user has time to read each entry; the fade animation on
  // .rp-button-label covers the visual transition.
  const cycleList = hasUnavailable ? unavailableList : selectedList
  const [cycleIdx, setCycleIdx] = useState(0)
  useEffect(() => {
    setCycleIdx(0)
    if (cycleList.length <= 1) return
    const id = window.setInterval(() => {
      setCycleIdx((i) => (i + 1) % cycleList.length)
    }, 5000)
    return () => window.clearInterval(id)
  }, [cycleList])

  // Open / close.
  const handleOpen = useCallback(() => setOpen((v) => !v), [])
  useHotkey(config.style?.hotkey?.open, handleOpen)

  useEffect(() => {
    if (!open) return
    inputRef.current?.focus()
    inputRef.current?.select()
  }, [open])

  // Click-outside-to-close. We listen on the host document; composedPath()
  // crosses the Shadow DOM boundary so we can tell whether the original
  // click target lives inside our root.
  useEffect(() => {
    if (!open) return
    const handler = (e: MouseEvent) => {
      const root = rootRef.current
      if (!root) return
      if (e.composedPath().includes(root)) return
      setOpen(false)
    }
    document.addEventListener('mousedown', handler, true)
    return () => document.removeEventListener('mousedown', handler, true)
  }, [open])

  // Fetch tuples (debounced via reqRef counter to avoid out-of-order updates).
  useEffect(() => {
    if (!open) return
    const myReq = ++reqRef.current
    const handle = window.setTimeout(() => {
      listTuples(config, query)
        .then((resp) => {
          if (myReq !== reqRef.current) return
          setItems(resp.items ?? [])
          setCursor(0)
        })
        .catch(() => {
          if (myReq !== reqRef.current) return
          setItems([])
        })
    }, 120)
    return () => window.clearTimeout(handle)
  }, [open, query, config])

  // Build fuzzy index for client-side scoring + highlighting on top of
  // the server's substring filter.
  const rows: TupleRow[] = useMemo(() => {
    if (items.length === 0) return []
    if (!query) {
      return items.map((entry) => ({ entry, matches: [] as number[] }))
    }
    const fuzzy = createFuzzySearch(items, {
      getText: (i: TupleEntry) => [i.tuple],
    })
    const scored = fuzzy(query)
    return scored.map((res) => {
      const matches: number[] = []
      for (const range of res.matches[0] ?? []) {
        for (let i = range[0]; i <= range[1]; i++) matches.push(i)
      }
      return { entry: res.item, matches }
    })
  }, [items, query])

  const commit = useCallback(
    (entry: TupleEntry) => {
      const cookieName = entry.sourceCookie || config.sourceCookie
      if (!cookieName) return
      setEntry(cookieName, entry.routingKey, entry.alternative)
      setEntries(parse(readCookie(cookieName)))
    },
    [config],
  )

  // Keyboard navigation inside the panel.
  const onSearchKeyDown = useCallback(
    (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false)
        return
      }
      if (e.key === 'ArrowDown') {
        e.preventDefault()
        setCursor((c) => Math.min(c + 1, Math.max(rows.length - 1, 0)))
        return
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault()
        setCursor((c) => Math.max(c - 1, 0))
        return
      }
      if (e.key === 'Enter') {
        e.preventDefault()
        const row = rows[cursor]
        if (row) commit(row.entry)
        return
      }
    },
    [rows, cursor, commit],
  )

  const reload = useCallback(() => window.location.reload(), [])

  // Closed-state label. Cycles through the active selection so a
  // multi-tier override (e.g. demo.web=canary AND demo.db=laptop) is
  // visible without opening the panel. When at least one selected
  // variant is unreachable, the cycle narrows to those rows and the
  // label gains a generic "unavailable" prefix — "host PC" wording is
  // avoided because RemoteRoute upstreams can be anything (a laptop,
  // a staging box, an external service).
  let buttonLabel: string
  let buttonTitle: string
  if (hasUnavailable) {
    const e = cycleList[cycleIdx % cycleList.length]
    buttonLabel = `unavailable · ${e.routingKey}=${e.alternative}`
    buttonTitle =
      unavailableList.length === 1
        ? 'Selected route is currently unreachable; traffic to it will fail.'
        : `${unavailableList.length} selected routes are currently unreachable; traffic to them will fail.`
  } else if (selectedList.length > 0) {
    const e = cycleList[cycleIdx % cycleList.length]
    buttonLabel = `${e.routingKey}=${e.alternative}`
    buttonTitle =
      selectedList.length === 1
        ? `routing override active · ${e.routingKey}=${e.alternative}`
        : `${selectedList.length} routing overrides active`
  } else {
    buttonLabel = 'route: default'
    buttonTitle = 'no routing override active'
  }
  // When a hotkey is configured, append it to the tooltip so users see
  // the binding on hover. Two newlines so the binding visually
  // separates from the state copy in the native browser tooltip.
  const hotkeyHint = formatChord(config.style?.hotkey?.open)
  if (hotkeyHint) {
    buttonTitle = `${buttonTitle}\n\n${hotkeyHint} to toggle`
  }
  // Priority (highest wins):
  //   alarm        — at least one selected variant is unreachable
  //   needs-reload — own-tier variant drifted from page-render snapshot
  //   active       — at least one non-default override is set
  const stateClass = hasUnavailable
    ? 'rp-alarm'
    : needsReload
      ? 'rp-needs-reload'
      : hasAnyActive
        ? 'rp-active'
        : ''
  // Suppress isOwnActive — kept for parity with prior code paths but
  // no longer drives the label since selectedList covers the same info.
  void isOwnActive
  void ownVariant

  return (
    <div
      ref={rootRef}
      class={`rp-root rp-mode-${mode} rp-anchor-${anchor}`}
      style={hostStyle}
    >
      {open && (
        <div class="rp-panel">
          <div class="rp-header">
            <input
              ref={inputRef}
              class="rp-search"
              type="text"
              placeholder="fuzzy search…"
              value={query}
              onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
              onKeyDown={onSearchKeyDown}
              spellcheck={false}
              autocomplete="off"
            />
          </div>
          <TupleList
            rows={rows}
            cursor={cursor}
            entries={entries}
            onHover={setCursor}
            onSelect={commit}
          />
          {needsReload && (
            <div class="rp-banner">
              <span>own-tier variant changed — reload to apply</span>
              <button onClick={reload}>reload</button>
            </div>
          )}
        </div>
      )}
      {mode === 'sidebar' ? (
        <button
          class={`rp-tab ${stateClass}`}
          onClick={handleOpen}
          title={buttonTitle}
        >
          {/* Same cycling label as the floating button, just rotated
             with the sidebar's vertical writing-mode. Re-mounted on
             every label change (key={buttonLabel}) so rp-tab-label-in
             replays as a fade/slide on each rotation. */}
          <span key={buttonLabel} class="rp-tab-label">
            {buttonLabel}
          </span>
        </button>
      ) : (
        <button
          class={`rp-button ${stateClass}`}
          onClick={handleOpen}
          title={buttonTitle}
        >
          <span class="rp-dot" />
          {/* key={buttonLabel} re-mounts the span on every change so the
             rp-label-in keyframe replays — gives the rotation a soft
             fade/slide instead of a snap. Using the label string as the
             key (rather than cycleIdx) also covers selection edits. */}
          <span key={buttonLabel} class="rp-button-label">
            {buttonLabel}
          </span>
        </button>
      )}
    </div>
  )
}

function readEntries(cfg: WidgetConfig): Map<string, string> {
  if (!cfg.sourceCookie) return new Map()
  return parse(readCookie(cfg.sourceCookie))
}
