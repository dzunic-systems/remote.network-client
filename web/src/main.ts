import { createApp } from 'vue'
import PrimeVue from 'primevue/config'
import { definePreset } from '@primevue/themes'
import Aura from '@primevue/themes/aura'
import Button from 'primevue/button'
import InputText from 'primevue/inputtext'
import Select from 'primevue/select'
import Dialog from 'primevue/dialog'

import App from './App.vue'
import './style.css'

// The client wears the website's house system (the same preset the
// router dashboard uses): green-black ink, live-green signal. Aura is
// re-tinted so every PrimeVue surface lands on the site's palette.
const RemotePreset = definePreset(Aura, {
  semantic: {
    primary: {
      50: '#f2fbf6',
      100: '#e0f6e9',
      200: '#c2ecd6',
      300: '#9ee1bf',
      400: '#74d6a0', // site --green
      500: '#52b987',
      600: '#47976d', // site --green-dim
      700: '#3a7a58',
      800: '#2f6146',
      900: '#244b36',
      950: '#142c1f',
    },
    colorScheme: {
      dark: {
        surface: {
          0: '#ffffff',
          50: '#f4f8f6',
          100: '#e9ede9', // site --ink
          200: '#c6cec8',
          300: '#9ea8a1',
          400: '#7d8681', // site --ink-3
          500: '#5f6862',
          600: '#454d47',
          700: '#2b332d',
          800: '#1c2320',
          900: '#131a17', // site --surface-2
          950: '#0f1512', // site --surface
        },
        text: {
          color: '#e9ede9',
          hoverColor: '#ffffff',
          mutedColor: '#8b98a8',
          hoverMutedColor: '#b4bcb7',
        },
        content: {
          background: '#0f1512',
          hoverBackground: '#131a17',
          borderColor: '#1c2320',
        },
        overlay: {
          modal: { background: '#0f1512', borderColor: '#2b332d', color: '#e9ede9' },
          popover: { background: '#0f1512', borderColor: '#2b332d', color: '#e9ede9' },
          select: { background: '#0f1512', borderColor: '#2b332d', color: '#e9ede9' },
        },
        formField: {
          background: '#0b100e',
          borderColor: '#2b332d',
          hoverBorderColor: '#3a443c',
          focusBorderColor: '#74d6a0',
          color: '#e9ede9',
        },
      },
    },
  },
})

createApp(App)
  .use(PrimeVue, {
    theme: { preset: RemotePreset, options: { darkModeSelector: '.rn-dark' } },
  })
  .component('Button', Button)
  .component('InputText', InputText)
  .component('Select', Select)
  .component('Dialog', Dialog)
  .mount('#app')
