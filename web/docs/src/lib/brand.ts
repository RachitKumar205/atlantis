// Datum's light palette as hex, for the generated images: canvaskit and a
// standalone SVG cannot read the custom properties in web/shared/datum.css.
// Each value must change with its token there.
export const datum = {
  canvas0: "#FAFAF8",
  ink0: "#1B2019",
  ink1: "#3A4238",
  ink2: "#6B7268",
  line: "#BFBEAE",
  lineStrong: "#A8A796",
  accent: "#485F73",
} as const;

// The ring mark the docs header, the console rail and Cloud's sign-in draw
// inline, on their 26-unit viewBox.
export const mark = {
  grid: 26,
  outer: { r: 10, stroke: 1.3, color: datum.lineStrong },
  inner: { r: 5.5, stroke: 1.1, color: datum.ink2 },
  dot: { r: 1.9, color: datum.accent },
} as const;

// The atlantis favicon, on a 32-unit grid. These are the values of
// web/console/public/favicon.svg and web/cloud/public/favicon.svg, so the
// three products share one tab icon.
export const icon = {
  grid: 32,
  tile: { radius: 6, color: "#0C0C0E" },
  outer: { r: 12, stroke: 1.5, color: "#8E8E97" },
  inner: { r: 6.5, stroke: 1.2, color: "#C5C5CC" },
  dot: { r: 2.4, color: "#D4A574" },
} as const;

export function iconSvg(): string {
  const c = icon.grid / 2;
  return [
    `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${icon.grid} ${icon.grid}">`,
    `<rect width="${icon.grid}" height="${icon.grid}" rx="${icon.tile.radius}" fill="${icon.tile.color}"/>`,
    `<circle cx="${c}" cy="${c}" r="${icon.outer.r}" fill="none" stroke="${icon.outer.color}" stroke-width="${icon.outer.stroke}"/>`,
    `<circle cx="${c}" cy="${c}" r="${icon.inner.r}" fill="none" stroke="${icon.inner.color}" stroke-width="${icon.inner.stroke}"/>`,
    `<circle cx="${c}" cy="${c}" r="${icon.dot.r}" fill="${icon.dot.color}"/>`,
    `</svg>`,
    "",
  ].join("\n");
}
