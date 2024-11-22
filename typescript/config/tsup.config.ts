import { defineConfig } from "tsup";

export default defineConfig({
  // Target version (es2022, tu support viem + bigint literals)
  target: "es2022",
  // All of our entry-points
  entry: ["src/index.ts"],
  // Format waited
  format: ["cjs", "esm"],
  // Code splitting and stuff
  clean: true,
  splitting: true,
  minify: true,
  // Types config
  dts: {
    resolve: true,
  },
});
