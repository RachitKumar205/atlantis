import type { APIRoute } from "astro";
import { site } from "../config";

export const prerender = true;

// The llms.txt format: a title, a one-line summary, then linked sections.
export const GET: APIRoute = () =>
  new Response(
    [
      "# Atlantis",
      "",
      `> ${site.headline}. ${site.description}`,
      "",
      "## Start",
      "",
      `- [This page as markdown](${site.origin}/index.md): what Atlantis is, with real CLI output`,
      `- [Getting started](${site.install}): install tide, declare a schema, apply it`,
      `- [Documentation index](${site.docs}/llms.txt): every docs page, for agents`,
      "",
      "## Install",
      "",
      `- \`${site.installCommand}\``,
      "",
    ].join("\n"),
    { headers: { "Content-Type": "text/plain; charset=utf-8" } },
  );
