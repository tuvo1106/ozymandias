import { describe, expect, it, vi } from "vitest";
import { createDashboard, updateDashboard } from "./dashboardsApi";
import { problemsOf, saveFailure } from "./dashboardSave";
import { ApiError } from "./metricsApi";

const definition = { title: "t", widgets: [] };
const stored = { ...definition, id: 7, provisioned: false, created_at: "a", updated_at: "b" };

function answering(status: number, body: unknown) {
  return vi.fn(async () => new Response(JSON.stringify(body), { status }));
}

describe("createDashboard / updateDashboard", () => {
  it("POSTs a new definition and PUTs an existing one", async () => {
    const post = answering(201, stored);
    await expect(createDashboard(definition, post)).resolves.toEqual({ kind: "stored", dashboard: stored });
    const [url, init] = post.mock.calls[0] as unknown as [string, RequestInit];
    expect([url, init.method, JSON.parse(init.body as string)]).toEqual(["/api/v1/dashboards", "POST", definition]);

    const put = answering(200, stored);
    await updateDashboard(7, definition, put);
    const [purl, pinit] = put.mock.calls[0] as unknown as [string, RequestInit];
    expect([purl, pinit.method]).toEqual(["/api/v1/dashboards/7", "PUT"]);
  });

  // A 2xx means the row is written. Calling an unreadable answer a failure
  // invites a second save, which for a create is a second dashboard.
  it("reports a success it cannot read as a success", async () => {
    await expect(createDashboard(definition, answering(201, { odd: true }))).resolves.toEqual({ kind: "unreadable" });
  });

  it("rejects with the server's own sentence and status", async () => {
    const err = await updateDashboard(7, definition, answering(409, { error: "provisioned from a file" })).catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect([err.status, err.message]).toEqual([409, "provisioned from a file"]);
  });
});

describe("saveFailure", () => {
  it("gives each status its own state", () => {
    expect(saveFailure(new ApiError("bad", 400))).toEqual({ kind: "refused", message: "bad" });
    expect(saveFailure(new ApiError("uid taken", 409))).toEqual({ kind: "conflict", message: "uid taken" });
    expect(saveFailure(new ApiError("gone", 404))).toEqual({ kind: "gone" });
    expect(saveFailure(new ApiError("ozyd is unreachable: x"))).toEqual({ kind: "unreachable", message: "ozyd is unreachable: x" });
    expect(saveFailure(new ApiError("ours", 500))).toEqual({ kind: "failed", status: 500, message: "ours" });
    expect(saveFailure(new TypeError("boom"))).toEqual({ kind: "failed", status: undefined, message: "boom" });
    expect(saveFailure("?")).toEqual({ kind: "failed", status: undefined, message: "?" });
  });
});

describe("problemsOf", () => {
  it("splits the validator's joined errors into a list", () => {
    // The shape internal/dashboard's Validate produces, captured from it.
    const msg =
      "invalid dashboard: title is required\ninvalid dashboard: widgets[0] (a): layout x+w is 14, past the 12-column grid\ninvalid dashboard: widgets[0] (a) query[0]: col 7: expected a tag key but found the end of the query";
    expect(problemsOf(msg)).toEqual([
      "title is required",
      "widgets[0] (a): layout x+w is 14, past the 12-column grid",
      "widgets[0] (a) query[0]: col 7: expected a tag key but found the end of the query",
    ]);
    expect(problemsOf("the body is over 1 MiB")).toEqual(["the body is over 1 MiB"]);
  });
});
