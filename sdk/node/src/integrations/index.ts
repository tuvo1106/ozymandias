/**
 * Integration registry. The built-ins and third-party integrations go through
 * the same {@link registerIntegration}, which is the point: nothing a built-in
 * does is out of reach of an outside integration.
 *
 * There is intentionally no require-hook auto-patcher: see next.ts.
 *
 * @module
 */
import { globalState } from "../state.js";
import { fetchIntegration } from "./fetch.js";
import { nextIntegration } from "./next.js";
import { sqliteIntegration } from "./sqlite.js";
import type { Integration } from "./types.js";
import { winstonIntegration } from "./winston.js";

export type { Integration } from "./types.js";

const BUILTIN: Integration[] = [fetchIntegration, sqliteIntegration, nextIntegration, winstonIntegration];

/**
 * Registers an integration under its `name`, replacing a registered one of the
 * same name (a built-in can be overridden). Registering does not patch.
 *
 * @param integration - the integration.
 */
export function registerIntegration(integration: Integration): void {
  try {
    globalState().integrations.set(integration.name, integration);
  } catch {
    // Registration is best-effort.
  }
}

/**
 * Lists the available integrations (built-in and registered), by name.
 *
 * @returns integration names.
 */
export function listIntegrations(): string[] {
  const names = new Set(BUILTIN.map((i) => i.name));
  for (const n of globalState().integrations.keys()) names.add(n);
  return [...names];
}

function find(name: string): Integration | undefined {
  return globalState().integrations.get(name) ?? BUILTIN.find((i) => i.name === name);
}

/**
 * Patches the named integrations (all of them when `names` is omitted).
 * An unknown name, an unavailable target or a throwing `patch()` skips that
 * integration and never reaches the caller.
 *
 * @param names - integration names, or undefined for every one.
 * @returns the names that were patched.
 */
export function patchIntegrations(names?: string[]): string[] {
  const patched: string[] = [];
  for (const name of names ?? listIntegrations()) {
    try {
      const i = find(name);
      if (!i || !i.isAvailable()) continue;
      i.patch();
      patched.push(name);
    } catch {
      // A broken integration must not break startup or its siblings.
    }
  }
  return patched;
}

/**
 * Unpatches the named integrations (all when omitted). Never throws.
 *
 * @param names - integration names, or undefined for every one.
 */
export function unpatchIntegrations(names?: string[]): void {
  for (const name of names ?? listIntegrations()) {
    try {
      find(name)?.unpatch();
    } catch {
      // ignore
    }
  }
}
