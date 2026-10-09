// Shared helpers for the jqhelper service worker.

export const VERSION = "0.2.4";

// Stable error codes shared with the Go bridge (see internal/apierr).
export const CODES = {
  noExtension: "no_extension",
  notAuthenticated: "not_authenticated",
  notFound: "not_found",
  usage: "usage_error",
  api: "api_error",
  network: "network_error",
  timeout: "timeout",
  internal: "internal_error",
};

// jqError builds a plain error payload understood by the bridge.
export function jqError(code, message, details) {
  const e = new Error(message);
  e.code = code;
  if (details) e.details = details;
  return e;
}

// toError normalizes anything thrown into {code, message, details}.
export function toErrorPayload(err) {
  if (err && err.code) {
    return { code: err.code, message: err.message, details: err.details };
  }
  return { code: CODES.internal, message: String((err && err.message) || err) };
}

export function intOption(value, fallback) {
  const n = Number.parseInt(value, 10);
  return Number.isFinite(n) ? n : fallback;
}
