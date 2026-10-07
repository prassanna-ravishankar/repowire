"use client";

import { useState } from "react";
import { Branches, Patch, Phone, Slow } from "@lucasmarkes/hairline/react";
import Plate from "./Plate";

const FIGURES = { branches: Branches, patch: Patch, phone: Phone, slow: Slow };

export type PackageFigureName = keyof typeof FIGURES;

/** One of the @lucasmarkes/hairline figures, on a plate with its live read-out. */
export default function PackageFigure({ name, fig, caption, label }: { name: PackageFigureName; fig: string; caption: string; label: string }) {
  const [read, setRead] = useState("rest");
  const Figure = FIGURES[name];
  return (
    <Plate fig={fig} read={read} caption={caption}>
      <Figure className="plate-stage" label={label} onRead={setRead} />
    </Plate>
  );
}
