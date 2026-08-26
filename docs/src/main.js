import { createApp } from 'vue'
import App from './App.vue'

// tokens.css + site.css = the original docs/css/ stylesheets, moved verbatim
// (Hallmark · Manifesto · custom dark-industrial amber). Vite imports resolve
// them into the production bundle.
import './styles/tokens.css'
import './styles/site.css'

createApp(App).mount('#app')
