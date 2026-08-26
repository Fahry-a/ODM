import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// base './' → assets resolve relative to the page, so the site works at the
// repo root (github.io/ODM/) AND on the custom domain (odm.orynix.id).
export default defineConfig({
  plugins: [vue()],
  base: './',
})
