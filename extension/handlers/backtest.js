import { pageRun } from "./page.js";

export const backtestHandlers = {
  "backtest.run": (p) =>
    pageRun("backtest.run", {
      strategyId: p.strategyId,
      start: p.start,
      end: p.end,
      capital: p.capital,
      frequency: p.frequency || "day",
      compile: !!p.compile,
      useCredit: !!p.useCredit,
    }),

  "backtest.list": (p) =>
    pageRun("backtest.list", {
      strategyId: p.strategyId,
      status: p.status || "all",
      limit: p.limit || 50,
      compile: !!p.compile,
    }),

  "backtest.get": (p) => pageRun("backtest.get", { backtestId: p.backtestId }),
  "backtest.stats": (p) => pageRun("backtest.stats", { backtestId: p.backtestId }),

  "backtest.result": (p) =>
    pageRun("backtest.result", {
      backtestId: p.backtestId,
      offset: p.offset || 0,
      userRecordOffset: p.userRecordOffset || 0,
    }),

  "backtest.logs": (p) =>
    pageRun("backtest.logs", {
      backtestId: p.backtestId,
      offset: p.offset || 0,
      error: !!p.error,
      all: !!p.all,
    }),

  "backtest.trades": (p) => pageRun("backtest.trades", { backtestId: p.backtestId }),
  "backtest.positions": (p) => pageRun("backtest.positions", { backtestId: p.backtestId }),
  "backtest.monthly": (p) => pageRun("backtest.monthly", { backtestId: p.backtestId }),
  "backtest.cancel": (p) => pageRun("backtest.cancel", { backtestId: p.backtestId }),

  "backtest.delete": (p) =>
    pageRun("backtest.delete", { backtestId: p.backtestId, compile: !!p.compile }),
};
