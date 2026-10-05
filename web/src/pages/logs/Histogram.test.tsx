import { render, screen } from "@testing-library/react";
import type { Histogram as HistogramData } from "../../lib/logsApi";
import { Histogram } from "./Histogram";

const hour = 3_600_000;
const data = (over: Partial<HistogramData> = {}): HistogramData => ({
  from: 0,
  to: hour - 1,
  interval_ms: 60_000,
  buckets: [{ ts: hour - 60_000, counts: { info: 3 } }],
  truncated: false,
  ...over,
});

const left = (el: HTMLElement) => parseFloat(el.style.left);
const width = (el: HTMLElement) => parseFloat(el.style.width);

describe("Histogram", () => {
  it("lays bars out over the queried window, not over the data's own span", () => {
    render(<Histogram data={data()} onSelect={() => {}} />);
    const bar = screen.getByRole("button");
    // One minute of a one-hour window: a thin bar at the right edge, not a full-width block.
    expect(width(bar)).toBeLessThan(5);
    expect(left(bar)).toBeGreaterThan(95);
  });
  it("places two bars by their time", () => {
    render(<Histogram data={data({ buckets: [{ ts: 0, counts: { info: 1 } }, { ts: hour / 2, counts: { error: 1 } }] })} onSelect={() => {}} />);
    const [a, b] = screen.getAllByRole("button");
    expect(left(a!)).toBe(0);
    expect(left(b!)).toBeCloseTo(50, 0);
  });
  it("says so when there is nothing", () => {
    render(<Histogram data={data({ buckets: [] })} onSelect={() => {}} />);
    expect(screen.getByText("No logs in this range")).toBeInTheDocument();
  });
});
