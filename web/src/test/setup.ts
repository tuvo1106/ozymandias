import "@testing-library/jest-dom/vitest";
import { configure } from "@testing-library/react";

// findBy*/waitFor give up after 1s by default. `make ci` runs on the
// efficiency cores (taskpolicy -c background), where a render that takes
// 300ms normally has been measured past 1.4s, and two tests failed for it
// while passing every time at normal priority. The timeout only bounds how
// long a failing assertion waits; a passing one returns as soon as it holds.
configure({ asyncUtilTimeout: 5000 });
