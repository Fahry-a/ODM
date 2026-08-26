import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const root = resolve(here, '..')
const app = readFileSync(resolve(root, 'src/App.vue'), 'utf8')
const docs = readFileSync(resolve(root, 'src/pages/DocsPage.vue'), 'utf8')
const home = readFileSync(resolve(root, 'src/pages/HomePage.vue'), 'utf8')
const animation = readFileSync(resolve(root, 'src/components/DownloadAnimation.vue'), 'utf8')

const required = [
  [app, 'import DocsPage from \'./pages/DocsPage.vue\''],
  [app, '<DocsPage v-if="route === \'docs\'" />'],
  [home, '<main id="top">'],
  [home, '<DownloadAnimation />'],
  [docs, '<main id="docs" class="docs-page">'],
  [docs, 'ODM Documentation'],
  [docs, 'Operations playbook'],
  [docs, 'Security notes'],
  [docs, 'Documentation map'],
  [animation, 'Animated ODM chunk scheduler diagram'],
]

const missing = required.filter(([haystack, needle]) => !haystack.includes(needle)).map(([, needle]) => needle)
if (missing.length > 0) {
  console.error(`docs page contract missing: ${missing.join(', ')}`)
  process.exit(1)
}
