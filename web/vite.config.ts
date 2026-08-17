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
    // Bind all interfaces: in the compose stack the request arrives from
    // Caddy in another container, not from localhost.
    host: "0.0.0.0",
    // HMR has to be told what the BROWSER will connect to, not what the
    // server binds. Behind Caddy the page is https on :443 while vite thinks
    // it is http on :5173, so without this the HMR socket is advertised as
    // ws://localhost:5173, the browser refuses a plaintext socket from an
    // https page, and every edit needs a manual reload with "[vite] server
    // connection lost" as the only clue.
    //
    // Unset (bare `npm run dev`, or the published 127.0.0.1:25173 port) keeps
    // vite's own defaults, which are correct for that case.
    hmr: process.env.OPENRMM_PUBLIC_HOST
      ? {
          host: process.env.OPENRMM_PUBLIC_HOST,
          protocol: "wss",
          clientPort: 443,
        }
      : undefined,
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
