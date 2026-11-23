import type { APIRoute } from "astro";
import { renderCard } from "@atlantis/shared/raster";
import { site } from "../config";

export const prerender = true;

export const GET: APIRoute = async () =>
  new Response(
    await renderCard({
      title: site.headline,
      description: site.description,
      host: new URL(site.origin).host,
    }),
    { headers: { "Content-Type": "image/png" } },
  );
