// Central method router: maps bridge method names to handler functions.
import { CODES, jqError } from "../lib/protocol.js";
import { authHandlers } from "./auth.js";
import { strategyHandlers } from "./strategy.js";
import { folderHandlers } from "./folder.js";
import { backtestHandlers } from "./backtest.js";

const handlers = {
  ping: async () => ({ pong: true }),
  ...authHandlers,
  ...strategyHandlers,
  ...folderHandlers,
  ...backtestHandlers,
};

export async function handle(method, params) {
  const fn = handlers[method];
  if (!fn) throw jqError(CODES.usage, "未知方法: " + method);
  return fn(params || {});
}
