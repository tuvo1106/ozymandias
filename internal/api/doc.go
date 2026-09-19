// Package api serves ozyd's HTTP API (/api/v1/*, from M1) and the
// embedded web UI.
//
// The UI is a single-page app: the browser loads index.html once and the
// client-side router owns every path after that. So the server must answer an
// unknown path like /dashboards/abc with index.html, not 404, or a reload on
// any deep link would break. [UI] implements that, plus the caching split
// that makes rebuilds safe: Vite's hashed files under /assets/ never change
// and are cached for a year, while index.html (which names the current
// hashes) is never cached.
package api
