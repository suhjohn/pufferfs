// Pixel-art folder mark, drawn on a 64x64 grid with crisp edges. Uses
// currentColor so it inherits text color and stays aligned with the monochrome
// palette. Scales to any size while staying pixel-sharp.
const CANVAS_SIZE = 64;

export function PixelLogo({ size = 24 }: { size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox={`0 0 ${CANVAS_SIZE} ${CANVAS_SIZE}`}
      shapeRendering="crispEdges"
      aria-hidden="true"
      style={{ imageRendering: "pixelated", display: "block" }}
    >
      <path
        d="M14 18h12v4h6v4h20v14H12V22h2z"
        fill="currentColor"
        opacity="0.24"
      />
      <path
        d="M10 14h18v4h6v4h20v4h4v18H8V18h2zM14 18v4h-2v18h40V26H32v-4h-6v-4z"
        fill="currentColor"
        fillRule="evenodd"
      />
      <path
        d="M16 23h12v4h22v4H16z"
        fill="currentColor"
        opacity="0.46"
      />
      <path
        d="M16 34h38v4h2v8h-4v6h-4v2H14v-4h-4v-8h2v-6h4z"
        fill="currentColor"
        opacity="0.42"
      />
      <path
        d="M14 28h44v4h4v8h-2v8h-4v6h-4v4H10v-4H6V42h2v-8h2v-4h4zM16 34v2h-4v6h-2v8h4v4h34v-2h4v-6h4v-8h-2v-4z"
        fill="currentColor"
        fillRule="evenodd"
      />
      <path
        d="M18 34h36v4H17v4h-4v8h-2v-8h2v-4h5z"
        fill="currentColor"
        opacity="0.72"
      />
    </svg>
  );
}
