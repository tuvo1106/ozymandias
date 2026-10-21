import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { MonitorChart } from "./MonitorChart";
import { backtestAnswer } from "./testServer";

vi.mock("../../charts/TimeseriesChart", () => ({ TimeseriesChart: () => <div data-testid="chart" /> }));

describe("MonitorChart", () => {
  // The list holds every group's transitions only while none were cut.
  it("points at the list for undrawn groups only when it has their transitions", () => {
    const { rerender } = render(<MonitorChart backtest={{ ...backtestAnswer, series_omitted: 3 } as never} />);
    expect(screen.getByText(/3 more group\(s\) not drawn; their transitions are in the list/)).toBeInTheDocument();
    rerender(<MonitorChart backtest={{ ...backtestAnswer, series_omitted: 3, events_omitted: 9 } as never} />);
    expect(screen.queryByText(/in the list/)).toBeNull();
  });
});
