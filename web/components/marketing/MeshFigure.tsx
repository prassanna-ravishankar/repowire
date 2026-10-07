"use client";

import { useEffect, useRef, useState } from "react";
import HL from "./hairline/kernel";
import { mesh } from "./hairline/mesh";
import Plate from "./Plate";

/** The figure's own number at an intensity in [0, 1]: two straight lines that meet at 0.5, as the hairline bench maps it. */
function valueAt(i: number) {
  const [lo, mid, hi] = mesh.range;
  return i <= 0.5 ? lo + (i / 0.5) * (mid - lo) : mid + ((i - 0.5) / 0.5) * (hi - mid);
}

/** The hero figure: four terminals over a mesh, taking turns to send. Mounted on the vendored hairline kernel. */
export default function MeshFigure({ caption, intensity = 0.5 }: { caption: string; intensity?: number }) {
  const stageRef = useRef<HTMLDivElement>(null);
  const [read, setRead] = useState("rest");

  useEffect(() => {
    const stage = stageRef.current;
    if (!stage) return;
    HL.inject(document);
    const svg = HL.mk("svg", { viewBox: "0 0 400 320", "aria-hidden": "true" }, stage);
    let text = "rest";
    const readOut = {
      get textContent() { return text; },
      set textContent(v: string) { text = String(v ?? ""); setRead(text); },
    };
    const handle = mesh.mount({ stage, svg, read: readOut }, valueAt(intensity));
    return () => { handle.destroy(); svg.remove(); };
  }, [intensity]);

  return (
    <Plate read={read} caption={caption} className="plate-hero">
      <div ref={stageRef} className="plate-stage" data-hairline={mesh.name} role="img" aria-label={mesh.means} />
    </Plate>
  );
}
