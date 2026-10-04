import { getVisibleEntries } from "@cloudflare/nimbus-docs/runtime";
import type { APIRoute, GetStaticPaths, InferGetStaticPropsType } from "astro";
import { config } from "virtual:nimbus/config";
import { renderCard } from "@/lib/raster";

export const prerender = true;

// One card at /og/<entry id>.png for each page [...slug].astro renders: both
// enumerate getVisibleEntries, which getDocsStaticPaths reads.
export const getStaticPaths = (async () => {
  const entries = await getVisibleEntries();
  const sections = new Map(
    entries.filter((e) => !e.id.includes("/")).map((e) => [e.id, e.data.title]),
  );
  return entries.map((e) => {
    const [section, ...rest] = e.id.split("/");
    return {
      params: { slug: `${e.id}.png` },
      props: {
        eyebrow: rest.length ? sections.get(section) : undefined,
        title: e.data.title,
        description: e.data.description,
      },
    };
  });
}) satisfies GetStaticPaths;

type Props = InferGetStaticPropsType<typeof getStaticPaths>;

export const GET: APIRoute<Props> = async ({ props }) =>
  new Response(
    await renderCard({ ...props, host: new URL(config.site).host }),
    { headers: { "Content-Type": "image/png" } },
  );
