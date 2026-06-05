// Pixel-art puffer mark, drawn as 1x1 SVG cells with crisp edges. Uses
// currentColor for the body so it inherits text color, with the accent var for
// the eye. Scales to any size while staying pixel-sharp.
const ART = [
  "..#.#.#..",
  "#.#####.#",
  ".#######.",
  "##o######",
  "##oo#####",
  "#########",
  ".#######.",
  "#.#.#.#.#",
];

export function PixelLogo({ size = 24 }: { size?: number }) {
  const cols = ART[0].length;
  const rows = ART.length;
  return (
    <svg
      width={size}
      height={(size * rows) / cols}
      viewBox={`0 0 ${cols} ${rows}`}
      shapeRendering="crispEdges"
      aria-hidden="true"
      style={{ imageRendering: "pixelated", display: "block" }}
    >
      {ART.flatMap((row, y) =>
        [...row].map((c, x) =>
          c === "." ? null : (
            <rect
              key={`${x}-${y}`}
              x={x}
              y={y}
              width={1}
              height={1}
              fill={c === "o" ? "var(--accent)" : "currentColor"}
            />
          ),
        ),
      )}
    </svg>
  );
}
