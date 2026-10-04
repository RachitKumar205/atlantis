import fs from "node:fs/promises";
import { createRequire } from "node:module";
import InitCanvasKit from "canvaskit-wasm/full";
import type {
  Canvas,
  CanvasKit,
  Paragraph,
  Surface,
  TypefaceFontProvider,
} from "canvaskit-wasm";
import { datum, icon, mark } from "./brand";

// Social cards and raster icons, drawn at build time.
//
// Font paths are relative to web/docs, the working directory of both
// `astro dev` and `astro build`.

const require = createRequire(import.meta.url);

let kit: Promise<CanvasKit> | undefined;
let fonts: Promise<TypefaceFontProvider> | undefined;

function canvasKit(): Promise<CanvasKit> {
  kit ??= InitCanvasKit({
    locateFile: (file) => require.resolve(`canvaskit-wasm/bin/full/${file}`),
  });
  return kit;
}

function fontProvider(): Promise<TypefaceFontProvider> {
  fonts ??= (async () => {
    const ck = await canvasKit();
    const provider = ck.TypefaceFontProvider.Make();
    const faces = [
      ["Outfit", "src/assets/fonts/outfit-var-latin.woff2"],
      ["Manrope", "src/assets/fonts/manrope-var-latin.woff2"],
    ] as const;
    for (const [family, file] of faces) {
      provider.registerFont(await fs.readFile(file), family);
    }
    return provider;
  })();
  return fonts;
}

const weights = { 400: "Normal", 500: "Medium", 600: "SemiBold", 700: "Bold" } as const;

type Run = {
  text: string;
  family: "Outfit" | "Manrope";
  weight: keyof typeof weights;
  size: number;
  color: string;
  lineHeight?: number;
  // In em, as CSS letter-spacing.
  tracking?: number;
};

function paragraph(
  ck: CanvasKit,
  provider: TypefaceFontProvider,
  runs: Run[],
  width: number,
  maxLines?: number,
): Paragraph {
  const builder = ck.ParagraphBuilder.MakeFromFontProvider(
    new ck.ParagraphStyle({
      textStyle: {},
      ...(maxLines ? { maxLines, ellipsis: "…" } : {}),
    }),
    provider,
  );
  for (const run of runs) {
    // The faces are variable fonts, so the weight is set on the wght axis.
    builder.pushStyle(
      new ck.TextStyle({
        color: ck.parseColorString(run.color),
        fontFamilies: [run.family],
        fontSize: run.size,
        fontStyle: { weight: ck.FontWeight[weights[run.weight]] },
        fontVariations: [{ axis: "wght", value: run.weight }],
        heightMultiplier: run.lineHeight ?? 1.2,
        halfLeading: true,
        letterSpacing: (run.tracking ?? 0) * run.size,
      }),
    );
    builder.addText(run.text);
    builder.pop();
  }
  const built = builder.build();
  built.layout(width);
  builder.delete();
  return built;
}

type Ring = { r: number; stroke: number; color: string };

function drawRings(
  ck: CanvasKit,
  canvas: Canvas,
  cx: number,
  cy: number,
  scale: number,
  rings: { outer: Ring; inner: Ring; dot: { r: number; color: string } },
) {
  const paint = new ck.Paint();
  paint.setAntiAlias(true);
  paint.setStyle(ck.PaintStyle.Stroke);
  for (const ring of [rings.outer, rings.inner]) {
    paint.setColor(ck.parseColorString(ring.color));
    paint.setStrokeWidth(ring.stroke * scale);
    canvas.drawCircle(cx, cy, ring.r * scale, paint);
  }
  paint.setStyle(ck.PaintStyle.Fill);
  paint.setColor(ck.parseColorString(rings.dot.color));
  canvas.drawCircle(cx, cy, rings.dot.r * scale, paint);
  paint.delete();
}

const card = { width: 1200, height: 630, pad: 80 } as const;

export type Card = {
  // The section a page sits in, set above the title.
  eyebrow?: string;
  title: string;
  description?: string;
  // The host printed in the footer.
  host: string;
};

// renderCard draws a 1200×630 PNG for og:image and twitter:image.
export async function renderCard(c: Card): Promise<Uint8Array<ArrayBuffer>> {
  const ck = await canvasKit();
  const provider = await fontProvider();
  const { width: W, height: H, pad } = card;

  const surface = ck.MakeSurface(W, H);
  if (!surface) throw new Error("canvaskit: no surface");
  const canvas = surface.getCanvas();
  canvas.clear(ck.parseColorString(datum.canvas0));

  // The mark again at 26×, its outer ring cut by the right edge. Its left
  // edge sits at x=840, 60px right of the text column.
  drawRings(ck, canvas, W - 100, H / 2, 26, {
    outer: { r: mark.outer.r, stroke: 0.1, color: datum.line },
    inner: { r: mark.inner.r, stroke: 0.1, color: datum.lineStrong },
    dot: { r: mark.dot.r, color: datum.accent },
  });

  const markSize = 48;
  drawRings(ck, canvas, pad + markSize / 2, pad + markSize / 2, markSize / mark.grid, mark);
  const brand = paragraph(
    ck,
    provider,
    [{ text: "Atlantis", family: "Outfit", weight: 600, size: 36, color: datum.ink0, tracking: -0.02, lineHeight: 1 }],
    600,
  );
  canvas.drawParagraph(brand, pad + markSize + 18, pad + (markSize - brand.getHeight()) / 2);

  const column = 700;
  const eyebrow = c.eyebrow
    ? paragraph(ck, provider, [
        { text: c.eyebrow.toUpperCase(), family: "Manrope", weight: 700, size: 22, color: datum.accent, tracking: 0.08 },
      ], column)
    : undefined;

  // The largest size that fits the title in three lines.
  let title!: Paragraph;
  for (const size of [76, 68, 60]) {
    title?.delete();
    title = paragraph(ck, provider, [
      { text: c.title, family: "Outfit", weight: 600, size, color: datum.ink0, tracking: -0.025, lineHeight: 1.08 },
    ], column, 3);
    if (!title.didExceedMaxLines()) break;
  }
  const titleLines = title.getLineMetrics().length;

  // Title and description share four lines.
  //
  // A description cut short ends at a word; canvaskit's ellipsis cuts mid-word.
  const descriptionLines = 4 - Math.min(titleLines, 3);
  let description: Paragraph | undefined;
  const full = c.description?.trim();
  if (full) {
    const words = full.split(/\s+/);
    for (let n = words.length; n > 0; n--) {
      const text = n === words.length ? full : `${words.slice(0, n).join(" ").replace(/[\s,.;:—–-]+$/, "")}…`;
      description?.delete();
      description = paragraph(ck, provider, [
        { text, family: "Manrope", weight: 500, size: 30, color: datum.ink1, lineHeight: 1.4 },
      ], column);
      if (description.getLineMetrics().length <= descriptionLines) break;
    }
  }

  const footer = paragraph(ck, provider, [
    { text: c.host, family: "Manrope", weight: 600, size: 24, color: datum.ink2, tracking: 0.01 },
  ], column);
  canvas.drawParagraph(footer, pad, H - pad - footer.getHeight());

  const gaps = { eyebrow: 20, description: 28 };
  const blockHeight =
    (eyebrow ? eyebrow.getHeight() + gaps.eyebrow : 0) +
    title.getHeight() +
    (description ? gaps.description + description.getHeight() : 0);
  const top = pad + markSize;
  const bottom = H - pad - footer.getHeight();
  let y = top + (bottom - top - blockHeight) / 2;
  if (eyebrow) {
    canvas.drawParagraph(eyebrow, pad, y);
    y += eyebrow.getHeight() + gaps.eyebrow;
  }
  canvas.drawParagraph(title, pad, y);
  y += title.getHeight();
  if (description) {
    canvas.drawParagraph(description, pad, y + gaps.description);
  }

  for (const p of [brand, eyebrow, title, description, footer]) p?.delete();
  return encodePng(ck, surface);
}

// renderIcon draws the favicon tile as a size×size PNG.
export async function renderIcon(size: number, shape: "rounded" | "square" = "rounded"): Promise<Uint8Array<ArrayBuffer>> {
  const ck = await canvasKit();
  const surface = ck.MakeSurface(size, size);
  if (!surface) throw new Error("canvaskit: no surface");
  const canvas = surface.getCanvas();
  canvas.clear(ck.TRANSPARENT);

  const scale = size / icon.grid;
  const radius = shape === "rounded" ? icon.tile.radius * scale : 0;
  const tile = new ck.Paint();
  tile.setAntiAlias(true);
  tile.setColor(ck.parseColorString(icon.tile.color));
  canvas.drawRRect(ck.RRectXY(ck.XYWHRect(0, 0, size, size), radius, radius), tile);
  tile.delete();
  drawRings(ck, canvas, size / 2, size / 2, scale, icon);
  return encodePng(ck, surface);
}

// encodePng snapshots and disposes of surface.
function encodePng(ck: CanvasKit, surface: Surface): Uint8Array<ArrayBuffer> {
  const image = surface.makeImageSnapshot();
  const png = image.encodeToBytes(ck.ImageFormat.PNG, 100);
  image.delete();
  surface.dispose();
  if (!png) throw new Error("canvaskit: PNG encode failed");
  // The copy is typed over ArrayBuffer, which a Response body requires.
  return new Uint8Array(png);
}

// ico packs PNGs into one .ico file: a 6-byte header, a 16-byte entry per
// image, then the images.
//
// Each size is 1–256 pixels; 256 is stored as 0.
export function ico(images: { size: number; png: Uint8Array }[]): Uint8Array<ArrayBuffer> {
  for (const { size } of images) {
    if (!Number.isInteger(size) || size < 1 || size > 256) throw new Error(`ico: size ${size} is outside 1–256`);
  }
  const headerSize = 6 + 16 * images.length;
  const out = new Uint8Array(headerSize + images.reduce((n, i) => n + i.png.length, 0));
  const view = new DataView(out.buffer);
  view.setUint16(0, 0, true);
  view.setUint16(2, 1, true);
  view.setUint16(4, images.length, true);
  let offset = headerSize;
  images.forEach(({ size, png }, i) => {
    const entry = 6 + 16 * i;
    view.setUint8(entry, size % 256);
    view.setUint8(entry + 1, size % 256);
    view.setUint16(entry + 4, 1, true);
    view.setUint16(entry + 6, 32, true);
    view.setUint32(entry + 8, png.length, true);
    view.setUint32(entry + 12, offset, true);
    out.set(png, offset);
    offset += png.length;
  });
  return out;
}
