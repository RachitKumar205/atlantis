import type { APIRoute } from "astro";
import { iconSvg } from "@/lib/brand";

export const prerender = true;

export const GET: APIRoute = () =>
  new Response(iconSvg(), { headers: { "Content-Type": "image/svg+xml" } });
