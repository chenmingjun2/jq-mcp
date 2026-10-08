import { pageRun } from "./page.js";

export const strategyHandlers = {
  "strategy.list": (p) =>
    pageRun("strategy.list", {
      fid: p.fid,
      limit: p.limit || 50,
      all: !!p.all,
      sort: p.sort || "updated",
    }),

  "strategy.find": (p) => pageRun("strategy.find", { name: p.name }),

  "strategy.get": (p) =>
    pageRun("strategy.get", { strategyId: p.strategyId, includeCode: !!p.includeCode }),

  "strategy.create": (p) =>
    pageRun("strategy.create", {
      name: p.name,
      code: p.code,
      type: p.type || "stock",
      folderId: p.folderId,
    }),

  "strategy.save": (p) =>
    pageRun("strategy.save", { strategyId: p.strategyId, name: p.name, code: p.code }),

  "strategy.delete": (p) => pageRun("strategy.delete", { strategyId: p.strategyId }),

  "strategy.move": (p) => pageRun("strategy.move", { strategyIds: p.strategyIds, folderId: p.folderId }),
};
