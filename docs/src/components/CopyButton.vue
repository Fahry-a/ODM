<script setup>
import { ref } from 'vue'

const props = defineProps({ text: { type: String, required: true } })

const label = ref('Copy')

async function copy() {
  const done = () => {
    label.value = 'Copied'
    setTimeout(() => { label.value = 'Copy' }, 1600)
  }
  try {
    if (navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(props.text)
      done()
    } else {
      // no clipboard API (older http pages): fall back to a hidden textarea
      const ta = document.createElement('textarea')
      ta.value = props.text
      ta.style.position = 'fixed'
      ta.style.opacity = '0'
      document.body.appendChild(ta)
      ta.select()
      try {
        if (!document.execCommand('copy')) throw new Error('clipboard copy was rejected')
        done()
      } finally { document.body.removeChild(ta) }
    }
  } catch {
    label.value = 'Failed'
    setTimeout(() => { label.value = 'Copy' }, 1600)
  }
}
</script>

<template>
  <button class="copy-btn" type="button" :data-copy="text" @click="copy">{{ label }}</button>
</template>
