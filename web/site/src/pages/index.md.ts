import type { APIRoute } from "astro";
import { pageMarkdown } from "../markdown";

export const prerender = true;

export const GET: APIRoute = () =>
  new Response(pageMarkdown(), {
    headers: { "Content-Type": "text/markdown; charset=utf-8" },
  });
