# Passkeys

Headplane supports passkey (WebAuthn) login as an alternative to typing a
Headscale API key. Passkeys are phishing-resistant credentials that live in an
authenticator — a password manager with passkey support (for example LastPass),
your phone, or a hardware security key — instead of in your head.

## Enabling

Passkeys are enabled automatically on the Go server; no configuration is
required. To turn them off, add the `webauthn` section with `enabled: false`:

```yaml
webauthn:
  enabled: false
```

### Relying-party ID

The WebAuthn relying-party (RP) ID defaults to the hostname the browser used to
reach Headplane (the request's `Host` header). If Headplane sits behind a
reverse proxy and the public hostname differs from the `Host` header the Go
server sees, pin the RP ID explicitly:

```yaml
webauthn:
  rp_id: "headplane.example.com"
```

The value must be a bare hostname, not a URL.

## Registering a passkey

1. Sign in to Headplane with your existing method.
2. Go to **Settings → Passkeys** and click **Add passkey**.
3. Give the passkey a label (for example "LastPass") and follow your
   browser's prompt.

Each user can register multiple passkeys. Labels can be renamed later, and
passkeys can be deleted from the same page.

For the best experience, register the passkey with a **synced provider** such
as LastPass rather than a per-device credential, so the same passkey works on
every computer you use.

## Signing in

On the login page, click **Sign in with passkey** and follow your browser's
prompt. On a computer that does not have your passkey provider (for example a
machine without LastPass), the browser offers a QR code you can scan with your
phone to complete the sign-in cross-device.

Passkey sign-in issues the same `_hp_auth` session cookie as the other login
methods, and API-key login keeps working as a fallback.

## Security notes

- Registration requires an authenticated session; passkeys can only be added,
  renamed, or deleted by the signed-in user they belong to.
- Ceremonies use discoverable credentials (`residentKey: required`) with
  `userVerification: preferred` and a 120-second timeout.
- Challenge tokens are single-use and expire after five minutes.
- The authenticator's signature counter is tracked to detect cloned
  credentials.
