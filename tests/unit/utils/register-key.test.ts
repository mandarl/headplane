import { describe, expect, test } from "vitest";

import { normalizeRegistrationKey } from "~/utils/register-key";

const suffix = "ABCDEFGHIJKLMNOPQRSTUVWX";
const key = `hskey-authreq-${suffix}`;
const legacyKey = "mkey:0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";

describe("normalizeRegistrationKey", () => {
  test("keeps full registration keys", () => {
    expect(normalizeRegistrationKey(key)).toBe(key);
  });

  test("keeps legacy machine keys", () => {
    expect(normalizeRegistrationKey(legacyKey)).toBe(legacyKey);
  });

  test("trims surrounding whitespace", () => {
    expect(normalizeRegistrationKey(`  ${key}\n`)).toBe(key);
  });

  test("extracts keys from registration URLs", () => {
    expect(normalizeRegistrationKey(`https://headscale.example.com/register/${key}`)).toBe(key);
  });

  test("extracts legacy machine keys from registration URLs", () => {
    expect(normalizeRegistrationKey(`https://headscale.example.com/register/${legacyKey}`)).toBe(
      legacyKey,
    );
  });

  test("extracts keys from registration URLs with trailing URL parts", () => {
    expect(normalizeRegistrationKey(`https://headscale.example.com/register/${key}?foo=bar`)).toBe(
      key,
    );
  });

  test("extracts auth request keys from URLs with trailing punctuation", () => {
    expect(normalizeRegistrationKey(`https://headscale.example.com/register/${key}.`)).toBe(key);
  });

  test("accepts suffix-only input as a fallback", () => {
    expect(normalizeRegistrationKey(suffix)).toBe(key);
  });

  test("does not require an exact suffix length for full keys", () => {
    const longerKey = `${key}YZ12`;
    expect(normalizeRegistrationKey(longerKey)).toBe(longerKey);
  });

  test("rejects empty or unrelated input", () => {
    expect(normalizeRegistrationKey("   ")).toBeNull();
    expect(normalizeRegistrationKey("not-a-registration-key")).toBeNull();
    expect(normalizeRegistrationKey("hskey-authreq-short")).toBeNull();
    expect(normalizeRegistrationKey("https://headscale.example.com/register/not-a-key")).toBeNull();
  });
});
