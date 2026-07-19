const authRequestKeyPattern = /^hskey-authreq-[A-Za-z0-9_-]{16,}$/;
const authRequestKeySearchPattern = /hskey-authreq-[A-Za-z0-9_-]{16,}/;
const legacyMachineKeyPattern = /^mkey:[A-Za-z0-9_-]{16,}$/;
const registerUrlPattern = /\/register\/([^\s?#]+)/;
const registrationKeySuffixPattern = /^[A-Za-z0-9_-]{24,}$/;

export function normalizeRegistrationKey(value: string): string | null {
  const trimmed = value.trim();
  if (trimmed.length === 0) {
    return null;
  }

  const registerUrlMatch = trimmed.match(registerUrlPattern);
  if (registerUrlMatch) {
    const token = normalizeRegistrationToken(registerUrlMatch[1]);
    if (token) {
      return token;
    }
  }

  const authRequestKeyMatch = trimmed.match(authRequestKeySearchPattern);
  if (authRequestKeyMatch) {
    return authRequestKeyMatch[0];
  }

  return normalizeRegistrationToken(trimmed);
}

function normalizeRegistrationToken(value: string): string | null {
  let token: string;
  try {
    token = decodeURIComponent(value.trim());
  } catch {
    return null;
  }

  if (authRequestKeyPattern.test(token) || legacyMachineKeyPattern.test(token)) {
    return token;
  }

  if (registrationKeySuffixPattern.test(token)) {
    return `hskey-authreq-${token}`;
  }

  return null;
}
