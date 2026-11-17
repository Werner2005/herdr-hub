import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  build: {
    rollupOptions: {
      input: { app: "index.html", docs: "docs/index.html" },
    },
  },
  server: {
    host: "0.0.0.0",
    proxy: {
      "/api": "http://localhost:8080",
      "/healthz": "http://localhost:8080",
      "/docs/openapi.json": "http://localhost:8080",
    },
  },
});
