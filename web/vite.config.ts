import path from "node:path"
import tailwindcss from "@tailwindcss/vite"
import react from "@vitejs/plugin-react"
import { defineConfig } from "vite"

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },
  },
  server: {
    proxy: {
      // In the compose dev stack the API is reachable by service name;
      // for bare `npm run dev` it falls back to localhost.
      "/api": {
        target: process.env.OPENRMM_API_URL ?? "http://localhost:8000",
        changeOrigin: true,
        ws: true, // the remote shell rides a WebSocket through this proxy
      },
    },
  },
})
