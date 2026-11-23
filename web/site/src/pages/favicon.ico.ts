import type { APIRoute } from "astro";
import { ico, renderIcon } from "@atlantis/shared/raster";

export const prerender = true;

export const GET: APIRoute = async () => {
  const sizes = [16, 32, 48];
  const images = await Promise.all(
    sizes.map(async (size) => ({ size, png: await renderIcon(size) })),
  );
  return new Response(ico(images), {
    headers: { "Content-Type": "image/x-icon" },
  });
};
