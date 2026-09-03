import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import hostReact from './vite-plugin-host-react.mjs';

export default defineConfig({
  plugins: [hostReact(), react()],
  server: {
    port: 5174,
    strictPort: true,
    cors: true,
  },
  build: {
    target: 'es2022',
    sourcemap: true,
    lib: {
      entry: 'src/main.jsx',
      formats: ['es'],
      fileName: () => 'main.js',
    },
    outDir: 'dist',
    emptyOutDir: true,
  },
});
