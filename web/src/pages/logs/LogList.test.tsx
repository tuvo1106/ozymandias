import { fireEvent, render, screen } from "@testing-library/react";
import type { LogEntry } from "../../lib/logsApi";
import { formatTs, LogList, ROW_HEIGHT } from "./LogList";

const logs: LogEntry[] = Array.from({ length: 10_000 }, (_, i) => ({ ts: 1_790_000_000_000 - i, message: `row ${i}\nsecond line`, status: "info", service: "s" }));

describe("LogList", () => {
  it("renders only the rows in view, however many logs there are", () => {
    render(<LogList logs={logs} columns={[]} selected={undefined} onSelect={() => {}} onNearEnd={() => {}} height={240} />);
    const n = screen.getAllByRole("listitem").length;
    expect(n).toBeGreaterThan(5);
    expect(n).toBeLessThan(40);
    expect(screen.getByText("row 0")).toBeInTheDocument(); // first line only
    expect(screen.queryByText(/second line/)).not.toBeInTheDocument();
  });
  it("moves the window on scroll, and asks for more near the end only", () => {
    const near = vi.fn();
    render(<LogList logs={logs} columns={[]} selected={undefined} onSelect={() => {}} onNearEnd={near} height={240} />);
    expect(near).not.toHaveBeenCalled();
    fireEvent.scroll(screen.getByRole("list"), { target: { scrollTop: 5000 * ROW_HEIGHT } });
    expect(screen.getByText("row 5000")).toBeInTheDocument();
    expect(screen.queryByText("row 0")).not.toBeInTheDocument();
    expect(near).not.toHaveBeenCalled();
    fireEvent.scroll(screen.getByRole("list"), { target: { scrollTop: 9990 * ROW_HEIGHT } });
    expect(near).toHaveBeenCalled();
  });
  it("says so when nothing matches, and formats a timestamp in local time", () => {
    render(<LogList logs={[]} columns={[]} selected={undefined} onSelect={() => {}} onNearEnd={() => {}} />);
    expect(screen.getByText("No logs match.")).toBeInTheDocument();
    expect(formatTs(new Date(2026, 9, 4, 9, 5, 7, 42).getTime())).toBe("10-04 09:05:07.042");
  });
});
