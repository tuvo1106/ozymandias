import { describe, expect, it } from "vitest";
import type { TemplateVar } from "./dashboard";
import {
  DEFAULT_VIEW_STATE,
  bindVars,
  parseViewState,
  selectedValue,
  serializeViewState,
  type DashboardViewState,
} from "./dashboardState";

const parse = (qs: string) => parseViewState(new URLSearchParams(qs));

describe("parseViewState", () => {
  it("defaults to a live last hour with nothing chosen", () => {
    expect(parse("")).toEqual(DEFAULT_VIEW_STATE);
  });

  it("reads a preset, an absolute window and the live flag", () => {
    expect(parse("range=4h").range).toEqual({ kind: "relative", preset: "4h" });
    expect(parse("from=100&to=200").range).toEqual({ kind: "absolute", from: 100, to: 200 });
    expect(parse("live=0").live).toBe(false);
  });

  // Number("") is 0, so a blank ?from= would otherwise be a valid window
  // starting at the epoch and quietly query January 1970.
  it("ignores a half-written or backwards window", () => {
    expect(parse("from=&to=200").range).toEqual(DEFAULT_VIEW_STATE.range);
    expect(parse("from=300&to=200").range).toEqual(DEFAULT_VIEW_STATE.range);
    expect(parse("range=nonsense").range).toEqual(DEFAULT_VIEW_STATE.range);
  });

  // The distinction the whole encoding exists for.
  it("tells an unchosen variable from one cleared on purpose", () => {
    expect(parse("").vars.env).toBeUndefined();
    expect(parse("var.env=").vars.env).toBe("");
    expect(parse("var.env=prod").vars.env).toBe("prod");
  });

  // The lexer lower-cases a $name as it reads one and the API lower-cases
  // var.<name> to match, so a link that shouts still resolves.
  it("lower-cases variable names", () => {
    expect(parse("var.Env=prod").vars.env).toBe("prod");
  });

  it("ignores a bare var. with no name", () => {
    expect(parse("var.=prod").vars).toEqual({});
  });
});

describe("serializeViewState", () => {
  const roundTrip = (s: DashboardViewState) => parseViewState(serializeViewState(s));

  it("round-trips every state it can produce", () => {
    for (const state of [
      DEFAULT_VIEW_STATE,
      { ...DEFAULT_VIEW_STATE, range: { kind: "relative", preset: "5m" } as const },
      { ...DEFAULT_VIEW_STATE, range: { kind: "absolute", from: 1, to: 2 } as const },
      { ...DEFAULT_VIEW_STATE, live: false },
      { ...DEFAULT_VIEW_STATE, vars: { env: "prod", service: "" } },
    ]) {
      expect(roundTrip(state)).toEqual(state);
    }
  });

  it("omits what is already the default, so links stay short", () => {
    expect(serializeViewState(DEFAULT_VIEW_STATE).toString()).toBe("");
  });

  // Omitting this one would make a cleared selector snap back to the
  // definition's default on the next render, which makes it unusable.
  it("keeps a variable that was cleared on purpose", () => {
    expect(serializeViewState({ ...DEFAULT_VIEW_STATE, vars: { env: "" } }).toString()).toBe("var.env=");
  });

  it("writes variables in a stable order", () => {
    const params = serializeViewState({ ...DEFAULT_VIEW_STATE, vars: { z: "1", a: "2" } });
    expect(params.toString()).toBe("var.a=2&var.z=1");
  });
});

describe("selectedValue", () => {
  const env: TemplateVar = { name: "env", tag: "env", default: "prod" };

  it("prefers the URL over the definition's default", () => {
    expect(selectedValue(env, { ...DEFAULT_VIEW_STATE, vars: { env: "dev" } })).toBe("dev");
    expect(selectedValue(env, DEFAULT_VIEW_STATE)).toBe("prod");
  });

  it("treats a cleared selector as all, not as the default", () => {
    expect(selectedValue(env, { ...DEFAULT_VIEW_STATE, vars: { env: "" } })).toBe("");
  });

  it("reads * and an absent default as all", () => {
    expect(selectedValue({ name: "e", tag: "e", default: "*" }, DEFAULT_VIEW_STATE)).toBe("");
    expect(selectedValue({ name: "e", tag: "e" }, DEFAULT_VIEW_STATE)).toBe("");
  });

  it("matches a declaration that shouts against a lower-cased URL", () => {
    expect(selectedValue({ name: "Env", tag: "env" }, { ...DEFAULT_VIEW_STATE, vars: { env: "dev" } })).toBe("dev");
  });
});

describe("bindVars", () => {
  it("binds the chosen value as the tag the definition names", () => {
    const vars: TemplateVar[] = [{ name: "env", tag: "deployment_environment", default: "prod" }];
    expect(bindVars(vars, DEFAULT_VIEW_STATE)).toEqual({ env: ["deployment_environment:prod"] });
  });

  // An unbound $var is an error, not a silently wider query — so every
  // declared variable is sent even when it resolves to nothing.
  it("binds a cleared variable to no filter rather than leaving it out", () => {
    const vars: TemplateVar[] = [{ name: "env", tag: "env" }];
    expect(bindVars(vars, DEFAULT_VIEW_STATE)).toEqual({ env: [] });
  });

  it("is empty for a dashboard that declares nothing", () => {
    expect(bindVars(undefined, DEFAULT_VIEW_STATE)).toEqual({});
  });
});
