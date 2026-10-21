// Compile one strategy, then run a batch of backtests and stream results.
//
//   node clients/mjs/example.mjs <strategyId> [start] [end]
//
// Requires a running jqhelper browser extension (logged in) and bin/jq-mcp.
import { startClient } from "./jq-mcp-client.mjs";

const [strategyId, start = "2024-01-01", end = "2026-09-30"] = process.argv.slice(2);
if (!strategyId) {
  console.error("usage: node clients/mjs/example.mjs <strategyId> [start] [end]");
  process.exit(1);
}

const fmt = (v) => (typeof v === "number" ? v.toFixed(4) : String(v ?? "-"));

async function main() {
  const jq = await startClient({ requireLogin: true });
  const t0 = Date.now();
  try {
    console.log(`bridge ready in ${Date.now() - t0}ms`);

    // 1) Compile validation first — failures never reach the backtest queue.
    const compiled = await jq.compileAll(
      [{ tag: "compile", strategyId, start: "2024-01-02", end: "2024-01-10" }],
      {
        onJob: (r) =>
          console.log(
            `[compile] ${r.status} in ${r.elapsedMs ?? "?"}ms` +
              (r.errorLogs?.length ? `\n  ${r.errorLogs.slice(-2).join("\n  ")}` : ""),
          ),
      },
    );
    if (compiled[0]?.status !== "done") {
      console.error("compile failed, aborting");
      process.exitCode = 2;
      return;
    }

    // 2) Backtests: submit once, results arrive as each job finishes.
    const results = await jq.runAll(
      [
        { tag: "full", strategyId, start, end, capital: 100000, frequency: "day" },
        { tag: "recent", strategyId, start: "2024-01-01", end, capital: 100000, frequency: "day" },
      ],
      {
        submit: { maxConcurrent: 10 },
        onJob: (r) => {
          const m = r.metrics || {};
          console.log(
            `[${r.tag}] ${r.status} ${r.elapsedMs ?? "?"}ms ` +
              `ann=${fmt(m.annual_algo_return)} dd=${fmt(m.max_drawdown)} sharpe=${fmt(m.sharpe)}`,
          );
        },
      },
    );

    console.log(`\ndone in ${Date.now() - t0}ms, ${results.length} jobs, calls=${JSON.stringify(jq.stats.byTool)}`);
  } finally {
    await jq.close();
  }
}

main().catch((e) => {
  console.error("FAILED:", e);
  process.exit(1);
});
