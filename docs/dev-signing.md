# Signing dev builds on macOS

mobd needs Screen Recording and Accessibility. macOS ties those grants (TCC) to the binary's
code signature. This page explains why a rebuilt `bin/mobd` loses them and how dev builds keep
them. Release builds are signed in CI with the Developer ID certificate
([releases](releases.md)); nothing here changes them.

## Why grants disappear after a rebuild

`go build` produces an **ad-hoc** signature (identifier `a.out`, no certificate). Its designated
requirement is the binary's own hash:

```text
designated => cdhash H"d947a497…"
```

Every rebuild changes the hash, so macOS sees a new app. The old entry in System Settings may
still look switched on, but it no longer applies: `/health` reports `accessibility` and
`screenRecording` failing until the entry is removed and added again.

## Design

Sign dev builds with a fixed identifier and a certificate that stays the same across builds.
The designated requirement then no longer depends on the hash:

```text
designated => identifier "com.xrdavies.mobd" and certificate leaf = H"e9f9b8f5…"
```

Grant once; later rebuilds signed the same way keep the grant.

- **Certificate:** a self-signed code-signing certificate created on the Mac that runs dev
  builds. It is free, never leaves that Mac, and is enough for TCC. macOS marks it "not
  trusted" (`CSSMERR_TP_NOT_TRUSTED`); `codesign` still signs with it.
  - Not the release Developer ID certificate: its private key would have to be copied onto a
    dev machine.
  - An Xcode "Apple Development" certificate also works, but it expires yearly and needs an
    Apple ID on the machine.
- **A dedicated keychain:** the certificate's key lives in
  `~/Library/Keychains/mobd-dev.keychain-db`, whose random password is in
  `~/.mobd/mobd-dev.keychain.pass` (mode 0600). The login keychain is locked to SSH sessions,
  so signing from it over SSH fails with `errSecInternalComponent`; the dedicated keychain is
  unlocked by the build, and its key is set so `codesign` may use it without a prompt.
- **Signing step:** `tools/devsign.sh IDENTITY BINARY` unlocks the dedicated keychain, searches
  only that keychain while signing (a second copy of the identity in the locked login keychain
  would otherwise be picked and fail), restores the keychain search list, and signs with
  `--identifier com.xrdavies.mobd`. The Makefile calls it when `SIGN_IDENTITY` is set; a plain
  `make build` is unchanged, so CI and other machines need nothing.

## Usage

Every build after setup:

```bash
make build SIGN_IDENTITY=<certificate SHA-1 or name>
codesign -dvv bin/mobd    # Identifier=com.xrdavies.mobd, Authority=mobd dev
./bin/mobd stop; ./bin/mobd start
```

Prefer the SHA-1 (`security find-identity -p codesigning`) when several certificates share a
name. Restart from a login shell as usual, so the service finds `codex` and `claude`.

## One-time setup on a Mac

1. **Create the certificate** (at the Mac's screen): Keychain Access → Certificate Assistant →
   Create a Certificate. Name `mobd dev`, Identity Type "Self Signed Root", Certificate Type
   "Code Signing".
2. **Export it** (at the screen): Keychain Access → My Certificates → `mobd dev` → Export, as
   `~/.mobd/mobd-dev.p12` with an export password.
3. **Create the dedicated keychain and import** (works over SSH):

   ```bash
   umask 077
   KC=~/Library/Keychains/mobd-dev.keychain-db
   openssl rand -hex 24 > ~/.mobd/mobd-dev.keychain.pass
   KP=$(cat ~/.mobd/mobd-dev.keychain.pass)
   security create-keychain -p "$KP" "$KC"
   security set-keychain-settings "$KC"          # no auto-lock
   security unlock-keychain -p "$KP" "$KC"
   security import ~/.mobd/mobd-dev.p12 -k "$KC" -P '<export password>' -T /usr/bin/codesign
   security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$KP" "$KC"
   security list-keychains -d user -s ~/Library/Keychains/login.keychain-db "$KC"
   rm ~/.mobd/mobd-dev.p12
   ```

   Keep the login keychain first in the search list. Set the list with explicit paths: piping
   `security list-keychains` output back into `-s` can save a mangled entry.
4. **Build signed**, then in System Settings → Privacy & Security remove the old `mobd` entries
   under **Accessibility** and **Screen & System Audio Recording** (−) and add `bin/mobd` again
   (+). Restart mobd. This is the last re-grant for this certificate.

## Notes

- The grant follows the certificate. A new certificate, or a build signed differently (ad hoc,
  Developer ID), needs one more re-grant.
- The same approach works for any Apple app or tool under development that needs TCC grants:
  a stable bundle or code identifier plus a stable signing certificate.
