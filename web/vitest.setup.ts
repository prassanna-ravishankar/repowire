import "@testing-library/jest-dom/vitest";
import { afterEach, beforeEach } from "vitest";
import { cleanup } from "@testing-library/react";

// Fail any test that triggers console.error — e.g. React's render-phase
// "Cannot update a component while rendering a different component" warning.
// That class of issue is exactly what session-protection is trying to avoid,
// so silent passes here are not acceptable.
const originalError = console.error;
beforeEach(() => {
  console.error = (...args: unknown[]) => {
    originalError(...args);
    throw new Error(
      "console.error in test: " +
        args
          .map((a) => (a instanceof Error ? a.stack ?? a.message : String(a)))
          .join(" ")
    );
  };
});

afterEach(() => {
  console.error = originalError;
  cleanup();
});
