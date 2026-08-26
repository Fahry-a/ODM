<script setup>
defineProps({ compact: { type: Boolean, default: false } })

const chunks = Array.from({ length: 12 }, (_, index) => index + 1)
</script>

<template>
  <figure class="download-anim" :class="{ compact }" aria-label="Animated ODM chunk scheduler diagram">
    <div class="anim-topline">
      <span class="label">live model</span>
      <span class="tnum">x8 · smart · 5.0M/s</span>
    </div>
    <div class="anim-rail" aria-hidden="true">
      <span class="anim-pac">C</span>
      <span v-for="chunk in chunks" :key="chunk" class="anim-dot" :style="{ '--i': chunk - 1 }"></span>
    </div>
    <div class="anim-grid" aria-hidden="true">
      <span v-for="chunk in chunks" :key="`bar-${chunk}`" :style="{ '--i': chunk - 1 }"></span>
    </div>
    <figcaption>
      Chunk workers pull the next unfinished range, mirrors rotate per chunk, and the global rate bucket keeps aggregate speed predictable.
    </figcaption>
  </figure>
</template>
