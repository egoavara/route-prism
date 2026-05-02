import { StrictMode, useEffect, useState } from 'react'
import { createRoot } from 'react-dom/client'
import './index.css'
import { TooltipProvider } from '@/components/ui/tooltip'
import App from './App.tsx'
import { Shell, type View } from './Shell.tsx'
import WidgetPreview from './WidgetPreview.tsx'

const VIEW_HASH: Record<View, string> = {
  services: '',
  'widget-preview': '#widget-preview',
}

function viewFromHash(hash: string): View {
  return hash === '#widget-preview' ? 'widget-preview' : 'services'
}

function Root() {
  const [view, setView] = useState<View>(() => viewFromHash(window.location.hash))

  // Browser ↔ state sync. The hash is the source of truth so back/forward
  // navigation keeps the UI in step.
  useEffect(() => {
    const onHash = () => setView(viewFromHash(window.location.hash))
    window.addEventListener('hashchange', onHash)
    return () => window.removeEventListener('hashchange', onHash)
  }, [])

  const handleChange = (v: View) => {
    if (window.location.hash !== VIEW_HASH[v]) {
      window.location.hash = VIEW_HASH[v]
    }
    setView(v)
  }

  return (
    <TooltipProvider>
      <Shell view={view} onChangeView={handleChange}>
        {view === 'widget-preview' ? <WidgetPreview /> : <App />}
      </Shell>
    </TooltipProvider>
  )
}

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <Root />
  </StrictMode>,
)
