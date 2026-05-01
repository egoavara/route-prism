import { useEffect, useMemo, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Input } from '@/components/ui/input'

// WidgetPreview lets an operator interactively tweak widget options and see
// the rendered widget live in an isolated <iframe>. Source of truth is the
// operator's /widget/preview HTML page; we only build the right query
// string and reload the iframe on changes. Header chrome is supplied by
// the surrounding Shell — this component renders body only.

type FloatAnchor = 'top-left' | 'top-right' | 'bottom-left' | 'bottom-right'
type SidebarAnchor = 'left' | 'right'

interface FormState {
  target: string
  routingKey: string
  sourceCookie: string
  pathPrefix: string
  styleMode: 'float' | 'sidebar'
  floatAnchor: FloatAnchor
  sidebarAnchor: SidebarAnchor
  marginTop: string
  marginRight: string
  marginBottom: string
  marginLeft: string
  hotkeyOpen: string
}

const initialState: FormState = {
  target: 'web',
  routingKey: 'demo.web',
  sourceCookie: 'x-route-prism',
  pathPrefix: '.route-prism',
  styleMode: 'float',
  floatAnchor: 'bottom-right',
  sidebarAnchor: 'right',
  marginTop: '',
  marginRight: '16px',
  marginBottom: '16px',
  marginLeft: '',
  hotkeyOpen: 'ctrl+\\',
}

function buildSrc(state: FormState): string {
  const q = new URLSearchParams()
  q.set('target', state.target)
  q.set('routingKey', state.routingKey)
  q.set('sourceCookie', state.sourceCookie)
  q.set('pathPrefix', state.pathPrefix)
  q.set('style.mode', state.styleMode)
  q.set('style.anchor', state.styleMode === 'float' ? state.floatAnchor : state.sidebarAnchor)
  if (state.marginTop) q.set('style.margin.top', state.marginTop)
  if (state.marginRight) q.set('style.margin.right', state.marginRight)
  if (state.marginBottom) q.set('style.margin.bottom', state.marginBottom)
  if (state.marginLeft) q.set('style.margin.left', state.marginLeft)
  if (state.hotkeyOpen) q.set('style.hotkey.open', state.hotkeyOpen)
  return `/widget/preview?${q}`
}

interface WidgetPreviewProps {
  onBack?: () => void
}

export default function WidgetPreview({ onBack }: WidgetPreviewProps = {}) {
  const [state, setState] = useState<FormState>(initialState)
  const [debounced, setDebounced] = useState<FormState>(initialState)
  const update = <K extends keyof FormState>(key: K, value: FormState[K]) => {
    setState((s) => ({ ...s, [key]: value }))
  }
  const iframeRef = useRef<HTMLIFrameElement | null>(null)

  // Debounce so typing doesn't reload the iframe every keystroke.
  useEffect(() => {
    const t = setTimeout(() => setDebounced(state), 200)
    return () => clearTimeout(t)
  }, [state])

  const src = useMemo(() => buildSrc(debounced), [debounced])

  return (
    <>
      {onBack && (
        <div className="mx-auto flex w-full max-w-7xl px-6 pt-4">
          <Button variant="outline" size="sm" onClick={onBack}>
            ← Back
          </Button>
        </div>
      )}
    <main className="mx-auto grid min-h-0 w-full max-w-7xl flex-1 grid-cols-1 gap-6 p-6 lg:grid-cols-[minmax(0,360px)_1fr]">
      <Card className="flex h-full flex-col overflow-hidden">
        <CardHeader>
          <CardTitle className="text-base">options</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-1 flex-col gap-3 overflow-y-auto">
          <Field label="target service">
            <Input value={state.target} onChange={(e) => update('target', e.target.value)} />
          </Field>
          <Field label="routing key">
            <Input value={state.routingKey} onChange={(e) => update('routingKey', e.target.value)} />
          </Field>
          <Field label="source cookie">
            <Input
              value={state.sourceCookie}
              onChange={(e) => update('sourceCookie', e.target.value)}
            />
          </Field>
          <Field label="path prefix">
            <Input
              value={state.pathPrefix}
              onChange={(e) => update('pathPrefix', e.target.value)}
            />
          </Field>
          <Field label="style mode">
            <div className="flex gap-3 text-sm">
              {(['float', 'sidebar'] as const).map((m) => (
                <label key={m} className="flex items-center gap-1">
                  <input
                    type="radio"
                    name="style-mode"
                    checked={state.styleMode === m}
                    onChange={() => update('styleMode', m)}
                  />
                  {m}
                </label>
              ))}
            </div>
          </Field>
          {state.styleMode === 'float' ? (
            <Field label="anchor (corner)">
              <select
                className="rounded border bg-background px-2 py-1 text-sm"
                value={state.floatAnchor}
                onChange={(e) => update('floatAnchor', e.target.value as FloatAnchor)}
              >
                <option value="top-left">top-left</option>
                <option value="top-right">top-right</option>
                <option value="bottom-left">bottom-left</option>
                <option value="bottom-right">bottom-right</option>
              </select>
            </Field>
          ) : (
            <Field label="anchor (side)">
              <div className="flex gap-3 text-sm">
                {(['left', 'right'] as const).map((s) => (
                  <label key={s} className="flex items-center gap-1">
                    <input
                      type="radio"
                      name="sidebar-anchor"
                      checked={state.sidebarAnchor === s}
                      onChange={() => update('sidebarAnchor', s)}
                    />
                    {s}
                  </label>
                ))}
              </div>
            </Field>
          )}
          <div className="grid grid-cols-2 gap-3">
            <Field label="margin top">
              <Input
                value={state.marginTop}
                placeholder="e.g. 16px"
                onChange={(e) => update('marginTop', e.target.value)}
              />
            </Field>
            <Field label="margin right">
              <Input
                value={state.marginRight}
                placeholder="e.g. 16px"
                onChange={(e) => update('marginRight', e.target.value)}
              />
            </Field>
            <Field label="margin bottom">
              <Input
                value={state.marginBottom}
                placeholder="e.g. 16px"
                onChange={(e) => update('marginBottom', e.target.value)}
              />
            </Field>
            <Field label="margin left">
              <Input
                value={state.marginLeft}
                placeholder="e.g. 16px"
                onChange={(e) => update('marginLeft', e.target.value)}
              />
            </Field>
          </div>
          <Field label="hotkey">
            <Input
              value={state.hotkeyOpen}
              placeholder="ctrl+\\"
              onChange={(e) => update('hotkeyOpen', e.target.value)}
            />
          </Field>
        </CardContent>
      </Card>

      <Card className="flex h-full flex-col overflow-hidden">
        <CardHeader>
          <CardTitle className="text-base">preview</CardTitle>
        </CardHeader>
        <CardContent className="flex-1 p-0">
          <iframe
            ref={iframeRef}
            src={src}
            title="widget preview"
            className="size-full border-0 bg-white"
          />
        </CardContent>
      </Card>
    </main>
    </>
  )
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1 text-sm">
      <span className="text-muted-foreground">{label}</span>
      {children}
    </label>
  )
}
