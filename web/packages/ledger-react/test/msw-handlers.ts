import type { RequestHandler } from "msw";

// Shared default request handlers. Empty for now — feature tests append
// their own handlers via server.use(...) and reset after each test.
export const handlers: RequestHandler[] = [];
