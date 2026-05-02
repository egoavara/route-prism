import { render } from 'preact'
import { App } from './App'
import { installHeaderOverrides } from './headerOverride'
import widgetCss from './styles.css?inline'
import type { WidgetConfig } from './types'

// Bootstrap: read config from the script tag's data attribute, mount a
// Shadow DOM so host page CSS/JS can't reach in (or vice versa), inject
// the bundled CSS, and render the App.
//
// Idempotent: a second load (e.g. HMR) wipes the previous host element.

const SCRIPT_DATA_ATTR = 'data-route-prism-config'
const HOST_ID = 'route-prism-widget-host'

function readConfig(): WidgetConfig | null {
  // document.currentScript is null in module mode; walk script tags.
  const scripts = document.querySelectorAll(`script[${SCRIPT_DATA_ATTR}]`)
  for (const s of Array.from(scripts)) {
    const raw = s.getAttribute(SCRIPT_DATA_ATTR)
    if (!raw) continue
    try {
      return JSON.parse(raw) as WidgetConfig
    } catch (e) {
      console.error('[route-prism] failed to parse', SCRIPT_DATA_ATTR, e)
    }
  }
  return null
}

function mount(cfg: WidgetConfig) {
  // Tear down any prior host element so re-injection is idempotent.
  const prior = document.getElementById(HOST_ID)
  if (prior) prior.remove()

  const host = document.createElement('div')
  host.id = HOST_ID
  // `all: initial` resets every CSS property to its initial value so the
  // host page's body styles can't bleed in. The Shadow DOM root then
  // attaches our own scoped styles.
  host.style.cssText = 'all: initial'
  document.body.appendChild(host)

  const shadow = host.attachShadow({ mode: 'open' })
  const styleEl = document.createElement('style')
  styleEl.textContent = widgetCss
  shadow.appendChild(styleEl)

  const mountPoint = document.createElement('div')
  shadow.appendChild(mountPoint)

  render(<App config={cfg} />, mountPoint)
}

const cfg = readConfig()
if (cfg) {
  // Patch fetch / XHR before any app code can issue a request — the
  // override has to be in place by the first user click, and mounting
  // the panel waits for DOMContentLoaded which runs much later.
  installHeaderOverrides(cfg)
  // Defer until DOM is ready so we can append to <body>.
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => mount(cfg), { once: true })
  } else {
    mount(cfg)
  }
} else {
  console.warn('[route-prism] no widget config found on script tag')
}
