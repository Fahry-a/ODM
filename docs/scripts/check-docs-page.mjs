import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { dirname, resolve } from 'node:path'

const here = dirname(fileURLToPath(import.meta.url))
const root = resolve(here, '..')
const app = readFileSync(resolve(root, 'src/App.vue'), 'utf8')

const required = [
  'href: \'#/docs\'',
  'const docsSections = [',
  '<main v-if="route === \'docs\'" id="docs" class="docs-page">',
  'ODM Documentation',
  'RPC reference',
]

const missing = required.filter((needle) => !app.includes(needle))
if (missing.length > 0) {
  console.error(`docs page contract missing: ${missing.join(', ')}`)
  process.exit(1)
}
