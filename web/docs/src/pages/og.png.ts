import type { APIRoute } from "astro";
import { config } from "virtual:nimbus/config";
import { renderCard } from "@/lib/raster";

export const prerender = true;

// The home page's card, and the fallback for any page without its own.
export const GET: APIRoute = async () =>
  new Response(
    await renderCard({
      title: "Documentation",
      description: config.description,
      host: new URL(config.site).host,
    }),
    { headers: { "Content-Type": "image/png" } },
  );
