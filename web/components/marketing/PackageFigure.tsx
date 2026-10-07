"use client";

import { useState } from "react";
import { Branches, Patch, Slow } from "@lucasmarkes/hairline/react";
import Plate from "./Plate";

const FIGURES = { branches: Branches, patch: Patch, slow: Slow };

export type PackageFigureName = keyof typeof FIGURES;

/** One of the @lucasmarkes/hairline figures, on a plate with its live read-out. */
export default function PackageFigure({ name, caption, label }: { name: PackageFigureName; caption: string; label: string }) {
  const [read, setRead] = useState("rest");
  const Figure = FIGURES[name];
  return (
    <Plate read={read} caption={caption}>
      <Figure className="plate-stage" label={label} onRead={setRead} />
    </Plate>
  );
}
