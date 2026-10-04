import type { APIRoute } from "astro";
import { renderIcon } from "@/lib/raster";

export const prerender = true;

// 180×180, the size iOS uses on a 3× display.
//
// Square: iOS rounds the corners itself and paints transparent pixels black.
export const GET: APIRoute = async () =>
  new Response(await renderIcon(180, "square"), {
    headers: { "Content-Type": "image/png" },
  });
