import { defineConfig } from "astro/config";
import { site } from "./src/config";

export default defineConfig({
  site: site.origin,
  output: "static",
  // One page, so there is nothing to prefetch.
  prefetch: false,
});
