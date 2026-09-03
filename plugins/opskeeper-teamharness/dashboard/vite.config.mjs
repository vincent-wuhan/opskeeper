import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import hostReact from './vite-plugin-host-react.mjs';
import { resolve } from 'path';

// Dashboard plugin lib mode：entry = src/main.jsx，输出 dist/main.js。
// 不打包 React/ReactDOM（peerDependency，运行时由 Dashboard host 提供）。
// 不打包 zustand/mitt（运行时由 host 提供；vite-plugin-host-react 注入 externals）。
export default defineConfig({
  plugins: [hostReact(), react()],
  server: {
    port: 5173,
    strictPort: true,
    cors: true,
  },
  build: {
    target: 'es2022',
    sourcemap: true,
    lib: {
      entry: resolve(__dirname, 'src/main.jsx'),
      formats: ['es'],
      fileName: () => 'main.js',
    },
    rollupOptions: {
      output: {
        extend: true,
      },
    },
  },
});
