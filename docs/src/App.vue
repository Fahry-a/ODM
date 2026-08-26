<script setup>
import { computed, onMounted, onUnmounted, ref } from 'vue'
import DocsPage from './pages/DocsPage.vue'
import HomePage from './pages/HomePage.vue'

const navLinks = [
  { href: '#install', label: 'Install' },
  { href: '#quickstart', label: 'Quickstart' },
  { href: '#profiles', label: 'Profiles' },
  { href: '#rpc', label: 'RPC' },
  { href: '#/docs', label: 'Docs' },
  { href: 'https://github.com/Fahry-a/ODM', label: 'Source' },
]

const routeHash = ref(window.location.hash)
const route = computed(() => (routeHash.value === '#/docs' || routeHash.value.startsWith('#docs-') ? 'docs' : 'home'))

function syncRoute() {
  routeHash.value = window.location.hash
}

onMounted(() => window.addEventListener('hashchange', syncRoute))
onUnmounted(() => window.removeEventListener('hashchange', syncRoute))
</script>

<template>
<header class="nav-slab">
  <a class="slab-mark" href="#top">odm</a>
  <nav class="slab-nav" aria-label="Primary">
    <ul>
      <li v-for="link in navLinks" :key="link.href"><a :href="link.href">{{ link.label }}</a></li>
    </ul>
  </nav>
</header>

<DocsPage v-if="route === 'docs'" />
<HomePage v-else />

<footer class="foot-stmt">
  <p class="foot-stmt__line">One budget<em>.</em> Split it sensibly<em>.</em></p>
  <div class="foot-stmt__meta">
    <span class="wordmark">odm</span>
    <span class="links">
      <a href="https://github.com/Fahry-a/ODM">GitHub</a>
      <a href="https://aur.archlinux.org/packages/odm-bin">AUR</a>
      <a href="https://github.com/Fahry-a/ODM/releases/latest">Releases</a>
    </span>
    <span class="foot-stmt__fine">a Go program by Fahry-a · MIT · this page documents v1.7.0</span>
  </div>
</footer>
</template>
