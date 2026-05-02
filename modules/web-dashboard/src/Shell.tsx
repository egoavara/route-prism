import type { ReactNode } from 'react'

export type View = 'services' | 'widget-preview'

interface ShellProps {
  view: View
  onChangeView: (v: View) => void
  children: ReactNode
}

const TABS: Array<{ id: View; label: string }> = [
  { id: 'services', label: 'routing surface' },
  { id: 'widget-preview', label: 'widget preview' },
]

// Shell renders the persistent app chrome — logo, view-switching nav,
// GitHub link — and slots the per-view body underneath. The header is
// shrink-0; the body container is a flex-1 column so per-view layouts
// can use h-full / min-h-0 patterns without leaking past the viewport.
export function Shell({ view, onChangeView, children }: ShellProps) {
  return (
    <div className="flex h-screen flex-col bg-background text-foreground">
      <header className="shrink-0 border-b">
        <div className="mx-auto flex max-w-7xl items-center justify-between px-6 py-4">
          <div className="flex items-center gap-6">
            <div className="flex items-center gap-3">
              <img
                src={`${import.meta.env.BASE_URL}icons.svg`}
                alt=""
                width={32}
                height={32}
                className="rounded-md"
              />
              <h1 className="text-xl font-semibold tracking-tight">route-prism</h1>
            </div>
            <nav className="flex items-center gap-1 rounded-md border bg-muted/30 p-0.5">
              {TABS.map((t) => {
                const active = t.id === view
                return (
                  <button
                    key={t.id}
                    type="button"
                    onClick={() => onChangeView(t.id)}
                    className={[
                      'rounded px-3 py-1.5 text-sm font-medium transition-colors',
                      active
                        ? 'bg-background text-foreground shadow-sm'
                        : 'text-muted-foreground hover:text-foreground',
                    ].join(' ')}
                  >
                    {t.label}
                  </button>
                )
              })}
            </nav>
          </div>
          <a
            href="https://github.com/egoavara/route-prism"
            target="_blank"
            rel="noreferrer"
            className="text-sm text-muted-foreground hover:text-foreground"
          >
            GitHub
          </a>
        </div>
      </header>

      <div className="flex min-h-0 flex-1 flex-col">{children}</div>
    </div>
  )
}
