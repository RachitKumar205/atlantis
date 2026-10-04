import { defineConfig } from "astro/config";
import tailwindcss from "@tailwindcss/vite";
import nimbus, {
  defineConfig as defineNimbusConfig,
} from "@cloudflare/nimbus-docs";
import { tableScroll } from "@cloudflare/nimbus-docs/markdown";
import { atl } from "./src/shiki/atl";
import { tideout } from "./src/shiki/tideout";
import { datumCodeTheme } from "./src/shiki/datum-theme";

const nimbusConfig = defineNimbusConfig({
  // The canonical origin, with no trailing slash. Drives canonical URLs,
  // absolute OG image URLs, robots.txt, the sitemap, and every link in
  // /llms.txt.
  site: "https://docs.tryatlantis.dev",
  // The site name: the home page's <title>, its WebSite structured data, and
  // the suffix of every other page's <title>.
  title: "Atlantis docs",
  description:
    "Declare your database schema in .atl, apply it, and get a typed " +
    "client, migrations, and a serving API derived from the same source.",
  locale: "en",
  // The repository is private, so there is no source link to offer.
  github: null,
  socialImageAlt: "Atlantis documentation",
  // Nimbus links /favicon.svg. Google Search reads no SVG favicon, so the
  // raster icons are linked beside it.
  head: [
    { tag: "link", attrs: { rel: "icon", href: "/favicon.ico", sizes: "16x16 32x32 48x48" } },
    { tag: "link", attrs: { rel: "apple-touch-icon", href: "/apple-touch-icon.png" } },
  ],
});

export default defineConfig({
  // nimbus:adapter
  output: "static",
  // Nimbus sets markdown.shikiConfig itself and Astro merges this over it.
  // One theme rather than a light/dark pair: code sits on --code-bg in both,
  // and naming a theme here also turns off Nimbus's token classing, so Shiki
  // writes the colour into the token instead of a --shiki-light/dark pair.
  //
  // `langs` carries only the two grammars Shiki does not ship. Nimbus scans
  // the fences and supplies the rest; Astro concatenates the two lists.
  markdown: {
    shikiConfig: {
      theme: datumCodeTheme,
      langs: [atl, tideout],
    },
  },
  // Tailwind v4 via its Vite plugin (the integration Astro recommends for
  // Tailwind v4 — replaces the PostCSS plugin, which doesn't build under
  // Astro 7's Vite 8 bundler).
  //
  // tailwindcss and @tailwindcss/vite are pinned to exact 4.1.4 in
  // package.json, and web/console pins 4.3.3, so npm keeps a second copy
  // under web/docs/node_modules instead of hoisting one to the root. A
  // hoisted plugin resolves the root's vite 5.4 — the version web/console
  // and web/cloud build with — where createIdResolver does not exist, and
  // `astro build` dies on `M.createIdResolver is not a function`. Widening
  // either pin to a caret collapses the two copies into one again.
  vite: {
    plugins: [tailwindcss()],
  },
  // Hover-prefetch link targets so full-page navigations feel instant without
  // a client-side router.
  prefetch: {
    prefetchAll: true,
    defaultStrategy: "hover",
  },
  integrations: [
    nimbus(nimbusConfig, {
      // Authoring rules are opt-in by design — your repo, your taste. The
      // two below are the load-bearing pair: frontmatter has to validate
      // against the content schema for the page to render properly, and
      // broken internal links are 404s for your readers. Add the others
      // (heading hierarchy, code-block language, style, etc.) when you're
      // ready to enforce them — see `nimbus-docs lint --help`.
      rules: {
        "nimbus/frontmatter-shape": "error",
        "nimbus/internal-link": "error",
      },
      // Wrap wide tables so they scroll instead of overflowing the page
      // (styled by `.nb-table-scroll` in src/styles/prose.css).
      markdown: {
        hastPlugins: [tableScroll()],
      },
    }),
  ],
});
