import type { APIRoute } from "astro";
import { ico, renderIcon } from "@/lib/raster";

export const prerender = true;

// Google Search reads no SVG favicon. Its recommended size, larger than 48×48,
// comes from apple-touch-icon.png, which it also reads.
export const GET: APIRoute = async () => {
  const sizes = [16, 32, 48];
  const images = await Promise.all(
    sizes.map(async (size) => ({ size, png: await renderIcon(size) })),
  );
  return new Response(ico(images), {
    headers: { "Content-Type": "image/x-icon" },
  });
};
