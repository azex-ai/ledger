import { expect, test } from "vitest";
import { VERSION } from "../src/index";

test("package exposes VERSION", () => {
  expect(VERSION).toBe("0.0.0");
});
