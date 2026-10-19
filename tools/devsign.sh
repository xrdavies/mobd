#!/bin/bash
# Signs a dev build of mobd so macOS keeps its Screen Recording and Accessibility grants across
# rebuilds (an ad-hoc signature changes with every build). Usage: devsign.sh IDENTITY BINARY
#
# IDENTITY is a code-signing certificate (name or SHA-1) in a dedicated keychain that can be
# unlocked without the login password, so this works over SSH:
#   SIGN_KEYCHAIN (default ~/Library/Keychains/mobd-dev.keychain-db)
#   SIGN_KEYCHAIN_PASS_FILE (default ~/.mobd/mobd-dev.keychain.pass)
set -euo pipefail
identity=$1 bin=$2
keychain=${SIGN_KEYCHAIN:-$HOME/Library/Keychains/mobd-dev.keychain-db}
passfile=${SIGN_KEYCHAIN_PASS_FILE:-$HOME/.mobd/mobd-dev.keychain.pass}
security unlock-keychain -p "$(cat "$passfile")" "$keychain"

# codesign picks an identity from the first keychain that has it; a copy in the (over SSH,
# locked) login keychain fails with errSecInternalComponent. Search only ours while signing.
saved=()
while IFS= read -r line; do
	line=${line#"${line%%[![:space:]]*}"} # trim leading spaces
	line=${line#\"} line=${line%\"}
	[ -n "$line" ] && saved+=("$line")
done < <(security list-keychains -d user)
restore() { security list-keychains -d user -s "${saved[@]}"; }
trap restore EXIT
security list-keychains -d user -s "$keychain"
codesign --force --keychain "$keychain" --identifier com.xrdavies.mobd --sign "$identity" "$bin"
