import { Check, ChevronDown, ChevronUp, Copy, Cookie, Trash2, X } from 'lucide-react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Separator } from '@/components/ui/separator'
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/ui/tooltip'
import { listAlternatives, listServices, type ServiceItem } from '@/api'

const PAGE_SIZE = 100
const SELF_ALTERNATIVE = '.'

// Prism palette — same hues used by the icon (red/green/blue refraction
// rays). Cycled across alternatives to suggest divergent routes from a
// single source.
const PRISM_PALETTE = ['#ef4444', '#22c55e', '#3b82f6'] as const

const prismColor = (i: number): string => PRISM_PALETTE[i % PRISM_PALETTE.length]

interface CartItem {
  alternative: string
  // Captured at selection time so cart composition doesn't depend on
  // re-fetching the alternative metadata.
  sourceCookie?: string
}

type Cart = Record<string, CartItem>

interface AlternativesData {
  routingKey: string
  sourceCookie?: string
  alternatives: string[]
}

function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value)
  useEffect(() => {
    const t = setTimeout(() => setDebounced(value), delayMs)
    return () => clearTimeout(t)
  }, [value, delayMs])
  return debounced
}

interface AppProps {
  onOpenWidgetPreview?: () => void
}

export default function App({ onOpenWidgetPreview }: AppProps = {}) {
  const [search, setSearch] = useState('')
  const fuzzy = useDebouncedValue(search.trim(), 150)

  const [services, setServices] = useState<ServiceItem[]>([])
  const [nextCursor, setNextCursor] = useState<string | undefined>(undefined)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [selected, setSelected] = useState<string | null>(null)
  const [cart, setCart] = useState<Cart>({})

  const loadFirstPage = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await listServices({ fuzzy: fuzzy || undefined, limit: PAGE_SIZE })
      setServices(res.items)
      setNextCursor(res.nextCursor)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [fuzzy])

  useEffect(() => {
    void loadFirstPage()
  }, [loadFirstPage])

  const loadMore = useCallback(async () => {
    if (!nextCursor || loading) return
    setLoading(true)
    try {
      const res = await listServices({
        fuzzy: fuzzy || undefined,
        limit: PAGE_SIZE,
        cursor: nextCursor,
      })
      setServices((prev) => [...prev, ...res.items])
      setNextCursor(res.nextCursor)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }, [nextCursor, loading, fuzzy])

  const toggleCart = useCallback((target: string, alternative: string, sourceCookie?: string) => {
    setCart((prev) => {
      const next = { ...prev }
      if (alternative === SELF_ALTERNATIVE) {
        delete next[target]
        return next
      }
      if (next[target]?.alternative === alternative) {
        delete next[target]
        return next
      }
      next[target] = { alternative, sourceCookie }
      return next
    })
  }, [])

  const removeFromCart = useCallback((target: string) => {
    setCart((prev) => {
      const next = { ...prev }
      delete next[target]
      return next
    })
  }, [])

  const clearCart = useCallback(() => setCart({}), [])

  return (
    <>
      {onOpenWidgetPreview && (
        <div className="mx-auto flex w-full max-w-7xl justify-end px-6 pt-4">
          <Button variant="outline" size="sm" onClick={onOpenWidgetPreview}>
            Widget preview
          </Button>
        </div>
      )}
      {/* min-h-0 lets the grid shrink inside the parent flex column.
          pb-24 reserves room for the floating Selection so the bottom
          of the panels stays visible above it. */}
      <main className="mx-auto grid min-h-0 w-full max-w-7xl flex-1 grid-cols-1 gap-6 p-6 pb-24 lg:grid-cols-[minmax(0,360px)_1fr]">
        <ServiceListPanel
          services={services}
          loading={loading}
          error={error}
          selected={selected}
          cart={cart}
          onSelect={setSelected}
          search={search}
          onSearchChange={setSearch}
          hasMore={!!nextCursor}
          onLoadMore={loadMore}
        />
        <AlternativesPanel target={selected} cart={cart} onToggle={toggleCart} />
      </main>

      <FloatingSelection cart={cart} onRemove={removeFromCart} onClear={clearCart} />
    </>
  )
}

interface ServiceListPanelProps {
  services: ServiceItem[]
  loading: boolean
  error: string | null
  selected: string | null
  cart: Cart
  onSelect: (target: string) => void
  search: string
  onSearchChange: (s: string) => void
  hasMore: boolean
  onLoadMore: () => void
}

function ServiceListPanel(props: ServiceListPanelProps) {
  const {
    services,
    loading,
    error,
    selected,
    cart,
    onSelect,
    search,
    onSearchChange,
    hasMore,
    onLoadMore,
  } = props

  return (
    <Card className="flex h-full min-h-0 flex-col overflow-hidden">
      <CardHeader className="shrink-0 space-y-3 pb-3">
        <div className="flex items-center justify-between">
          <CardTitle className="text-base">Services</CardTitle>
          <Badge variant="secondary" className="font-mono text-xs">
            {services.length}
          </Badge>
        </div>
        <Input
          placeholder="Search (fuzzy)…"
          value={search}
          onChange={(e) => onSearchChange(e.target.value)}
        />
      </CardHeader>
      <Separator className="shrink-0" />
      <ScrollArea className="min-h-0 flex-1">
        <CardContent className="p-2">
          {error && <div className="px-2 py-3 text-sm text-destructive">{error}</div>}
          {!error && services.length === 0 && !loading && (
            <div className="px-2 py-6 text-center text-sm text-muted-foreground">No services</div>
          )}
          <ul className="flex flex-col gap-0.5">
            {services.map((s) => {
              const isSelected = selected === s.target
              const inCart = !!cart[s.target]
              return (
                <li key={s.target}>
                  <button
                    type="button"
                    onClick={() => onSelect(s.target)}
                    className={
                      'flex w-full items-center gap-2 rounded-md px-3 py-2 text-left transition-colors ' +
                      (isSelected
                        ? 'bg-primary text-primary-foreground'
                        : 'hover:bg-accent hover:text-accent-foreground')
                    }
                  >
                    <span className="flex-1 truncate font-mono text-sm">{s.target}</span>
                    {inCart && (
                      <span
                        className={
                          'inline-block h-2 w-2 shrink-0 rounded-full ' +
                          (isSelected ? 'bg-primary-foreground' : 'bg-emerald-500')
                        }
                        aria-label="in cart"
                      />
                    )}
                    <ETBadge cookieName={s.translator} muted={isSelected} />
                  </button>
                </li>
              )
            })}
          </ul>
          {hasMore && (
            <div className="p-2">
              <Button
                variant="outline"
                size="sm"
                className="w-full"
                onClick={onLoadMore}
                disabled={loading}
              >
                {loading ? 'Loading…' : 'Load more'}
              </Button>
            </div>
          )}
        </CardContent>
      </ScrollArea>
    </Card>
  )
}

interface ETBadgeProps {
  // cookieName present → EdgeTransformation is attached, badge shows the
  // cookie name in its tooltip. Absent → "no ET" warning state.
  cookieName?: string
  // muted=true adapts the badge to a dark background (selected service
  // row in the service list). Default styling assumes a light surface.
  muted?: boolean
}

// ETBadge is the single source of truth for "this routing target
// {has,doesn't have} an EdgeTransformation". Used in:
//   - the Services list (per-row indicator)
//   - the FloatingSelection cart (per-cart-entry indicator)
// so both surfaces look and behave identically.
function ETBadge({ cookieName, muted }: ETBadgeProps) {
  const hasET = !!cookieName
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          className={
            'inline-flex shrink-0 items-center gap-1 rounded-md border px-1.5 py-0.5 font-mono text-[10px] uppercase tracking-wider ' +
            (muted
              ? 'border-primary-foreground/30 text-primary-foreground/80'
              : hasET
                ? 'border-amber-500/40 bg-amber-500/10 text-amber-700 dark:text-amber-400'
                : 'border-muted-foreground/30 text-muted-foreground')
          }
        >
          <Cookie className="h-3 w-3" aria-hidden />
          {hasET ? 'ET' : 'no ET'}
        </span>
      </TooltipTrigger>
      <TooltipContent side="right">
        {hasET ? (
          <>
            EdgeTransformation attached
            <span className="ml-1 font-mono">cookie={cookieName}</span>
          </>
        ) : (
          <>No EdgeTransformation — routes via Baggage header only.</>
        )}
      </TooltipContent>
    </Tooltip>
  )
}

interface AlternativesPanelProps {
  target: string | null
  cart: Cart
  onToggle: (target: string, alternative: string, sourceCookie?: string) => void
}

function AlternativesPanel({ target, cart, onToggle }: AlternativesPanelProps) {
  const [data, setData] = useState<AlternativesData | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    if (!target) {
      setData(null)
      setError(null)
      return
    }
    let cancelled = false
    const run = async () => {
      setLoading(true)
      setError(null)
      try {
        const all: string[] = []
        let cursor: string | undefined
        let routingKey = target
        let sourceCookie: string | undefined
        do {
          const res = await listAlternatives(target, { limit: 500, cursor })
          all.push(...res.items.map((i) => i.target))
          cursor = res.nextCursor
          routingKey = res.routingKey || target
          if (res.sourceCookie) sourceCookie = res.sourceCookie
        } while (cursor)
        if (!cancelled) setData({ routingKey, sourceCookie, alternatives: all })
      } catch (e) {
        if (!cancelled) setError(e instanceof Error ? e.message : String(e))
      } finally {
        if (!cancelled) setLoading(false)
      }
    }
    void run()
    return () => {
      cancelled = true
    }
  }, [target])

  if (!target) {
    return (
      <Card className="flex h-full min-h-0 flex-col items-center justify-center overflow-hidden">
        <CardContent className="flex flex-col items-center gap-4 text-center">
          <img
            src={`${import.meta.env.BASE_URL}icons.svg`}
            alt=""
            width={64}
            height={64}
            className="opacity-80"
          />
          <p className="text-sm text-muted-foreground">
            Select a service to inspect its routing alternatives.
          </p>
        </CardContent>
      </Card>
    )
  }

  const alternatives = data?.alternatives.filter((a) => a !== SELF_ALTERNATIVE) ?? []
  const hasSelf = data?.alternatives.includes(SELF_ALTERNATIVE) ?? false
  const cartChoice = cart[target]?.alternative

  return (
    <Card className="flex h-full min-h-0 flex-col overflow-hidden">
      {/* Header + ET banner stay pinned at the top; only the Routes
          list scrolls when alternatives overflow. */}
      <CardHeader className="shrink-0">
        <div className="flex items-center justify-between gap-3">
          <div className="min-w-0">
            <div className="text-xs uppercase tracking-wider text-muted-foreground">Target</div>
            <CardTitle className="truncate font-mono text-lg">{target}</CardTitle>
          </div>
          <Badge variant="secondary" className="font-mono">
            {alternatives.length} alt
          </Badge>
        </div>
      </CardHeader>
      <Separator className="shrink-0" />
      {error && <div className="shrink-0 px-6 pt-4 text-sm text-destructive">{error}</div>}
      {data && (
        <div className="shrink-0 px-6 pt-4">
          <div
            className={
              'flex items-start gap-3 rounded-md border px-3 py-2.5 text-sm ' +
              (data.sourceCookie
                ? 'border-amber-500/40 bg-amber-500/10'
                : 'border-border bg-muted/40')
            }
          >
            <Cookie
              className={
                'mt-0.5 h-4 w-4 shrink-0 ' +
                (data.sourceCookie ? 'text-amber-600 dark:text-amber-400' : 'text-muted-foreground')
              }
              aria-hidden
            />
            <div className="min-w-0 flex-1 space-y-0.5">
              {data.sourceCookie ? (
                <>
                  <div className="font-medium">EdgeTransformation attached</div>
                  <div className="text-xs text-muted-foreground">
                    Clients can route by cookie{' '}
                    <code className="font-mono">{data.sourceCookie}</code> or by Baggage header.
                  </div>
                </>
              ) : (
                <>
                  <div className="font-medium">No EdgeTransformation</div>
                  <div className="text-xs text-muted-foreground">
                    Clients must set the <code className="font-mono">Baggage</code> header directly
                    — cookie-based routing is unavailable for this target.
                  </div>
                </>
              )}
            </div>
          </div>
        </div>
      )}
      <ScrollArea className="min-h-0 flex-1">
        <CardContent className="space-y-2 pt-4">
          <h3 className="text-sm font-medium">Routes</h3>
          {loading && !data && <p className="text-sm text-muted-foreground">Loading…</p>}
          {data && (
            <ul className="flex flex-col gap-1.5">
              <li>
                <RouteOption
                  label={SELF_ALTERNATIVE}
                  description="self · unmarked traffic"
                  selected={!cartChoice}
                  onClick={() => onToggle(target, SELF_ALTERNATIVE)}
                  disabled={!hasSelf}
                  marker="self"
                />
              </li>
              {alternatives.map((alt, i) => (
                <li key={alt}>
                  <RouteOption
                    label={alt}
                    selected={cartChoice === alt}
                    onClick={() => onToggle(target, alt, data.sourceCookie)}
                    markerColor={prismColor(i)}
                  />
                </li>
              ))}
            </ul>
          )}
          {data && alternatives.length === 0 && (
            <p className="text-sm text-muted-foreground">No alternative routes for this target.</p>
          )}
        </CardContent>
      </ScrollArea>
    </Card>
  )
}

interface RouteOptionProps {
  label: string
  description?: string
  selected: boolean
  onClick: () => void
  disabled?: boolean
  marker?: 'self'
  markerColor?: string
}

function RouteOption(props: RouteOptionProps) {
  const { label, description, selected, onClick, disabled, marker, markerColor } = props
  return (
    <button
      type="button"
      onClick={onClick}
      disabled={disabled}
      className={
        'flex w-full items-center gap-3 rounded-md border px-3 py-2 text-left transition-colors ' +
        (selected
          ? 'border-primary bg-primary/5'
          : 'hover:border-foreground/30 hover:bg-accent/50') +
        (disabled ? ' cursor-not-allowed opacity-50' : '')
      }
    >
      <span
        aria-hidden
        className={
          'flex h-4 w-4 shrink-0 items-center justify-center rounded-full border ' +
          (selected ? 'border-primary' : 'border-muted-foreground/40')
        }
      >
        {selected && <span className="h-2 w-2 rounded-full bg-primary" />}
      </span>
      {marker === 'self' ? (
        <span className="inline-block h-2 w-2 shrink-0 rounded-full bg-emerald-500" aria-hidden />
      ) : (
        <span
          aria-hidden
          className="inline-block h-2 w-2 shrink-0 rounded-full"
          style={{ backgroundColor: markerColor }}
        />
      )}
      <span className="min-w-0 flex-1 truncate font-mono text-sm">{label}</span>
      {description && <span className="text-xs text-muted-foreground">{description}</span>}
    </button>
  )
}

interface FloatingSelectionProps {
  cart: Cart
  onRemove: (target: string) => void
  onClear: () => void
}

// FloatingSelection is anchored bottom-right and stays out of the main
// layout so the Services + Alternatives panels can dominate the page.
// Collapsed, it shows just the two routing strings users typically need
// (Baggage / Cookie). Expanded, it grows upward to reveal the cart
// contents and the curl example.
function FloatingSelection({ cart, onRemove, onClear }: FloatingSelectionProps) {
  const [expanded, setExpanded] = useState(false)
  const entries = useMemo(() => Object.entries(cart).sort(([a], [b]) => a.localeCompare(b)), [cart])

  const baggage = useMemo(
    () => entries.map(([rk, { alternative }]) => `${rk}=${alternative}`).join(','),
    [entries],
  )

  const cookieGroups = useMemo(() => {
    const groups: Record<string, Array<[string, string]>> = {}
    const missing: string[] = []
    for (const [rk, { alternative, sourceCookie }] of entries) {
      if (!sourceCookie) {
        missing.push(rk)
        continue
      }
      ;(groups[sourceCookie] ??= []).push([rk, alternative])
    }
    return {
      cookies: Object.entries(groups).map(([name, pairs]) => ({
        name,
        value: pairs.map(([rk, v]) => `${rk}:${v}`).join('|'),
      })),
      missing,
    }
  }, [entries])

  // Multiple Cookie groups (different cookie names) are concatenated
  // with "; " — that's what the actual Cookie request header looks like.
  const cookieHeader = cookieGroups.cookies.map((c) => `${c.name}=${c.value}`).join('; ')

  const curlExample = useMemo(() => {
    if (entries.length === 0) return ''
    const lines = ['curl \\']
    if (cookieHeader) lines.push(`  -H 'Cookie: ${cookieHeader}' \\`)
    if (baggage) lines.push(`  -H 'Baggage: ${baggage}' \\`)
    lines.push('  http://your-service/')
    return lines.join('\n')
  }, [entries.length, cookieHeader, baggage])

  const isEmpty = entries.length === 0

  return (
    <div className="fixed inset-x-4 bottom-4 z-50 flex justify-end sm:inset-x-auto sm:right-4">
      <div className="w-full max-w-[520px] overflow-hidden rounded-xl border bg-background/95 shadow-lg backdrop-blur supports-[backdrop-filter]:bg-background/80">
        {/* Header bar: always clickable to toggle expansion. */}
        <button
          type="button"
          onClick={() => setExpanded((v) => !v)}
          className="flex w-full items-center justify-between gap-2 px-4 py-2.5 text-left transition-colors hover:bg-accent/40"
        >
          <div className="flex items-center gap-2">
            <span className="text-sm font-medium">Selection</span>
            <Badge variant="secondary" className="font-mono text-xs">
              {entries.length}
            </Badge>
          </div>
          <div className="flex items-center gap-1">
            {/* Clear is rendered with visibility toggling instead of
                conditional mounting so the header bar's height/width
                stays identical between empty and populated states. */}
            <Button
              variant="ghost"
              size="sm"
              onClick={(e) => {
                e.stopPropagation()
                onClear()
              }}
              disabled={isEmpty}
              className={'h-7 gap-1 px-2 text-xs ' + (isEmpty ? 'invisible' : '')}
              tabIndex={isEmpty ? -1 : 0}
            >
              <Trash2 className="h-3.5 w-3.5" />
              Clear
            </Button>
            {expanded ? (
              <ChevronDown className="h-4 w-4 text-muted-foreground" />
            ) : (
              <ChevronUp className="h-4 w-4 text-muted-foreground" />
            )}
          </div>
        </button>

        {/* Collapsed body: always two rows so the floating card's shape
            never changes between empty and populated. Empty state shows
            "(no selection)" in place of the value. */}
        {!expanded && (
          <>
            <Separator />
            <div className="space-y-1.5 px-4 py-3">
              <CompactRow label="Baggage" value={baggage} placeholder="(no selection)" />
              <CompactRow
                label="Cookie"
                value={cookieHeader}
                placeholder={isEmpty ? '(no selection)' : '(no EdgeTransformation in selection)'}
              />
            </div>
          </>
        )}

        {/* Expanded body: cart items + full snippets (Baggage, Cookie
            groups, curl). max-h with overflow keeps it bounded on small
            screens. */}
        {expanded && (
          <>
            <Separator />
            <div className="max-h-[70vh] space-y-4 overflow-y-auto px-4 py-4">
              {isEmpty ? (
                <p className="rounded-md border border-dashed px-3 py-6 text-center text-sm text-muted-foreground">
                  Selection is empty. Click an alternative to add it here.
                </p>
              ) : (
                <ul className="grid grid-cols-1 gap-1.5">
                  {entries.map(([rk, item]) => (
                    <li
                      key={rk}
                      className="flex items-center gap-2 rounded-md border bg-card px-3 py-1.5"
                    >
                      <div className="min-w-0 flex-1 font-mono text-sm">
                        <span className="truncate text-muted-foreground">{rk}</span>
                        <span className="mx-1.5 text-muted-foreground">→</span>
                        <span className="truncate">{item.alternative}</span>
                      </div>
                      <ETBadge cookieName={item.sourceCookie} />
                      <button
                        type="button"
                        onClick={() => onRemove(rk)}
                        className="text-muted-foreground hover:text-foreground"
                        aria-label={`Remove ${rk}`}
                      >
                        <X className="h-3.5 w-3.5" />
                      </button>
                    </li>
                  ))}
                </ul>
              )}

              <div className="space-y-3">
                <SnippetRow label="Baggage header" value={baggage} />
                {cookieGroups.cookies.length === 0 ? (
                  <SnippetRow
                    label="Cookie"
                    value=""
                    placeholder="(no EdgeTransformation in selection)"
                  />
                ) : (
                  cookieGroups.cookies.map((c) => (
                    <SnippetRow
                      key={c.name}
                      label={
                        <>
                          Cookie <code className="font-mono">{c.name}</code>
                        </>
                      }
                      value={c.value}
                    />
                  ))
                )}
                {cookieGroups.missing.length > 0 && (
                  <p className="text-xs text-muted-foreground">
                    <span className="font-medium">Cookie unavailable for:</span>{' '}
                    {cookieGroups.missing.join(', ')}{' '}
                    <span>
                      — these targets have no EdgeTransformation, use the Baggage header instead.
                    </span>
                  </p>
                )}
                <SnippetRow
                  label="curl"
                  value={curlExample}
                  multiline
                  placeholder="(empty — pick at least one alternative)"
                />
              </div>
            </div>
          </>
        )}
      </div>
    </div>
  )
}

interface CompactRowProps {
  label: string
  value: string
  placeholder?: string
}

// CompactRow is the single-line view used in the collapsed FloatingSelection.
// Truncates long values; the full string lives in the expanded view.
function CompactRow({ label, value, placeholder = '(empty)' }: CompactRowProps) {
  // readOnly <input> is preferred over a truncated <code> here:
  //   - native horizontal scroll inside the box (drag, arrow keys)
  //   - native text selection (drag-select / Ctrl+A) without copy click
  //   - matches the visual look of <code> via styling
  // The tooltip surfaces the full value on hover for at-a-glance reads
  // when the user doesn't want to interact.
  const display = value || placeholder
  return (
    <div className="flex items-center gap-2">
      <span className="w-16 shrink-0 text-xs font-medium uppercase tracking-wider text-muted-foreground">
        {label}
      </span>
      <Tooltip>
        <TooltipTrigger asChild>
          <input
            type="text"
            readOnly
            value={display}
            aria-label={`${label} value`}
            className={
              'min-w-0 flex-1 rounded bg-muted/40 px-2 py-1 font-mono text-xs outline-none focus:ring-1 focus:ring-ring ' +
              (value ? '' : 'text-muted-foreground')
            }
            onFocus={(e) => e.currentTarget.select()}
          />
        </TooltipTrigger>
        {value && (
          <TooltipContent side="top" align="end" className="max-w-[480px] break-all font-mono">
            {value}
          </TooltipContent>
        )}
      </Tooltip>
      <CopyButton value={value} />
    </div>
  )
}

interface SnippetRowProps {
  label: React.ReactNode
  value: string
  multiline?: boolean
  placeholder?: string
}

function SnippetRow({ label, value, multiline, placeholder = '(empty)' }: SnippetRowProps) {
  return (
    <div className="space-y-1.5">
      <div className="flex items-center justify-between gap-2">
        <div className="text-xs font-medium uppercase tracking-wider text-muted-foreground">
          {label}
        </div>
        <CopyButton value={value} />
      </div>
      {multiline ? (
        // Reserve the same height the populated state would occupy
        // (4 lines: curl \, Cookie, Baggage, URL) so the empty → filled
        // transition does not nudge surrounding content.
        <pre className="min-h-[5.75rem] overflow-x-auto rounded-md border bg-muted/40 px-3 py-2 text-xs leading-relaxed">
          {value || <span className="text-muted-foreground">{placeholder}</span>}
        </pre>
      ) : (
        <div className="min-h-[2.5rem] overflow-x-auto rounded-md border bg-muted/40 px-3 py-2 font-mono text-sm">
          {value || <span className="text-muted-foreground">{placeholder}</span>}
        </div>
      )}
    </div>
  )
}

interface CopyButtonProps {
  value: string
}

function CopyButton({ value }: CopyButtonProps) {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<number | null>(null)

  useEffect(() => {
    return () => {
      if (timerRef.current !== null) window.clearTimeout(timerRef.current)
    }
  }, [])

  const onClick = useCallback(async () => {
    if (!value) return
    try {
      await navigator.clipboard.writeText(value)
      setCopied(true)
      if (timerRef.current !== null) window.clearTimeout(timerRef.current)
      timerRef.current = window.setTimeout(() => setCopied(false), 1200)
    } catch {
      // Clipboard API unavailable (insecure context, permission denied).
      // Fail silently — manual select-and-copy still works.
    }
  }, [value])

  return (
    <Button
      type="button"
      variant="ghost"
      size="sm"
      onClick={onClick}
      disabled={!value}
      className="h-7 gap-1.5 px-2 text-xs"
    >
      {copied ? (
        <>
          <Check className="h-3.5 w-3.5" /> Copied
        </>
      ) : (
        <>
          <Copy className="h-3.5 w-3.5" /> Copy
        </>
      )}
    </Button>
  )
}
