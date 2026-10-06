import React from 'react'
import ReactDOM from 'react-dom/client'

// fonts.css first: @font-face has to be declared before the rules that name
// the families. tokens.css still carries Bathysphere for anything not yet
// converted; datum.css declares the new palette under `.datum`, which the
// shell carries — see web/shared/datum.css.
import '@/styles/fonts.css'
import '@atlantis/shared/tokens.css'
import '@atlantis/shared/datum.css'
import '@atlantis/shared/otp.css'
import '@/styles/app.css'

import { App } from '@/App'

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)
