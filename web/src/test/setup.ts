import "@testing-library/jest-dom/vitest";
import { configure } from "@testing-library/react";

// findBy*/waitFor give up after 1s by default. `make ci` runs on the
// efficiency cores (taskpolicy -c background), where two tests timed out
// (one ran 1.47s) while passing every time at normal priority. The timeout only bounds how
// long a failing assertion waits; a passing one returns as soon as it holds.
// vite.config.ts raises testTimeout above it to match.
configure({ asyncUtilTimeout: 5000 });
