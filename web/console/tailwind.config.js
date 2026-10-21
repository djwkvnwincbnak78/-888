/** @type {import('tailwindcss').Config} */
export default {
  darkMode: 'class',
  content: ['./index.html', './src/**/*.{vue,ts}'],
  theme: {
    extend: {
      colors: {
        nx: {
          bg: '#09090b',
          surface: '#0f1014',
          border: '#1e2230',
          primary: '#8b5cf6',
          accent: '#22d3ee',
          text: '#e5e7eb',
          dim: '#94a3b8'
        }
      },
      boxShadow: {
        glow: '0 0 40px -12px rgba(139,92,246,0.55)'
      }
    }
  },
  plugins: []
}
