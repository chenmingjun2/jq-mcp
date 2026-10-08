import { CODES, jqError } from "../lib/protocol.js";
import { pageRun, ensureTab, joinquantCookies, cookieHeader } from "./page.js";

export const authHandlers = {
  "auth.status": async () => pageRun("auth.status", {}),

  "auth.getCookie": async () => {
    const cookies = await joinquantCookies();
    if (!cookies.length) {
      throw jqError(CODES.notAuthenticated, "未找到聚宽 Cookie，请先在浏览器登录聚宽");
    }
    return { cookie: cookieHeader(cookies), count: cookies.length };
  },

  "auth.openLogin": async (params) => {
    const url = params.url || "https://www.joinquant.com/user/login/index";
    const tab = await chrome.tabs.create({ url, active: true });
    return { tabId: tab.id, url };
  },

  "auth.refresh": async () => {
    const tab = await ensureTab();
    await chrome.tabs.reload(tab.id);
    await new Promise((r) => setTimeout(r, 1500));
    return pageRun("auth.status", {});
  },

  "account.limits": async () => pageRun("account.limits", {}),
};
