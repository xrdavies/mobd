#!/bin/sh
# Published by CI with the download origin and Developer ID team pinned below.
set -eu

main() {
  download_base='@DOWNLOAD_BASE_URL@'
  apple_team='@APPLE_TEAM_ID@'
  install_dir="$HOME/.local/bin"
  requested_version=''
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --version|--install-dir)
        [ "$#" -ge 2 ] || { printf 'Missing value for %s\n' "$1" >&2; return 1; }
        case "$1" in --version) requested_version="$2" ;; --install-dir) install_dir="$2" ;; esac
        shift 2 ;;
      --help|-h)
        printf 'Install mobd for macOS Apple Silicon.\nUsage: sh install.sh [--version X.Y.Z] [--install-dir /absolute/path]\n'
        return ;;
      *) printf 'Unknown option: %s\n' "$1" >&2; return 1 ;;
    esac
  done
  [ "$(id -u)" -ne 0 ] || { printf 'Run this installer as your normal user, without sudo.\n' >&2; return 1; }
  [ "$(uname -s)" = Darwin ] || { printf 'mobd currently supports macOS only.\n' >&2; return 1; }
  [ "$(uname -m)" = arm64 ] || [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ] || {
    printf 'mobd requires an Apple Silicon Mac.\n' >&2; return 1;
  }
  os_major=$(sw_vers -productVersion | cut -d. -f1)
  [ "$os_major" -ge 13 ] || { printf 'mobd requires macOS 13 or newer.\n' >&2; return 1; }
  case "$install_dir" in /*) ;; *) printf 'Installation directory must be an absolute path.\n' >&2; return 1 ;; esac
  running() { launchctl print "gui/$(id -u)/com.xrdavies.mobd" >/dev/null 2>&1 || pgrep -u "$(id -u)" -x mobd >/dev/null 2>&1; }
  if running; then
    printf 'mobd is running. Finish active tasks, run mobd stop (or stop mobd serve), then rerun this installer.\nUse mobd start --login afterwards if you want login startup.\n' >&2
    return 1
  fi
  temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/mobd-install.XXXXXX")
  staged=''
  trap 'rm -rf "$temp_dir"; if [ -n "$staged" ]; then rm -f "$staged"; fi' EXIT
  trap 'exit 1' HUP INT TERM
  fetch() { curl --proto '=https' --tlsv1.2 --retry 3 -fsSL "$1" -o "$2"; }
  if [ -z "$requested_version" ]; then
    fetch "$download_base/mobd/latest.json" "$temp_dir/latest.json"
    requested_version=$(plutil -extract version raw -o - "$temp_dir/latest.json")
  fi
  printf '%s\n' "$requested_version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || {
    printf 'Invalid stable version. Expected X.Y.Z.\n' >&2; return 1;
  }
  prefix="$download_base/mobd/v$requested_version"
  archive="mobd_${requested_version}_darwin_arm64.tar.gz"
  fetch "$prefix/release.json" "$temp_dir/release.json"
  [ "$(plutil -extract version raw -o - "$temp_dir/release.json")" = "$requested_version" ] || { printf 'Release version mismatch.\n' >&2; return 1; }
  expected=$(plutil -extract platforms.darwin-aarch64.sha256 raw -o - "$temp_dir/release.json")
  printf '%s\n' "$expected" | grep -Eq '^[0-9a-f]{64}$' || { printf 'Invalid release checksum.\n' >&2; return 1; }
  printf 'Downloading mobd %s…\n' "$requested_version"
  fetch "$prefix/$archive" "$temp_dir/$archive"
  actual=$(shasum -a 256 "$temp_dir/$archive" | cut -d ' ' -f1)
  [ "$actual" = "$expected" ] || { printf 'Checksum mismatch. Installation stopped.\n' >&2; return 1; }
  # Only regular files with these exact names may be extracted into the temporary directory.
  tar -tzf "$temp_dir/$archive" > "$temp_dir/contents"
  [ "$(LC_ALL=C sort "$temp_dir/contents")" = "$(printf 'LICENSE\nREADME.md\nmobd')" ] || { printf 'Unexpected archive contents.\n' >&2; return 1; }
  tar -tvzf "$temp_dir/$archive" | awk 'substr($0,1,1) != "-" {bad=1} END {exit bad}' || { printf 'Archive contains links or special files.\n' >&2; return 1; }
  tar -xzf "$temp_dir/$archive" -C "$temp_dir" mobd
  codesign --verify --strict --verbose=2 -R "=anchor apple generic and identifier \"com.xrdavies.mobd\" and certificate leaf[subject.OU] = \"$apple_team\" and certificate leaf[field.1.2.840.113635.100.6.1.13] exists" "$temp_dir/mobd"
  chmod 755 "$temp_dir/mobd"
  [ "$("$temp_dir/mobd" --version)" = "mobd $requested_version" ] || { printf 'Binary version mismatch.\n' >&2; return 1; }
  mkdir -p "$install_dir"
  [ ! -d "$install_dir/mobd" ] || { printf 'Installation target is a directory: %s/mobd\n' "$install_dir" >&2; return 1; }
  staged=$(mktemp "$install_dir/.mobd-install.XXXXXX")
  install -m 755 "$temp_dir/mobd" "$staged"
  if running; then
    printf 'mobd started during installation. Stop it before retrying.\n' >&2
    return 1
  fi
  mv -f "$staged" "$install_dir/mobd"
  staged=''
  printf '\nInstalled mobd %s to %s/mobd\n' "$requested_version" "$install_dir"
  printf 'Keep this directory on PATH: %s\n' "$install_dir"
  printf 'Next: mobd init (first install), edit ~/.mobd/config.json, then mobd start.\n'
  printf 'For login startup, use mobd start --login. Existing configuration and conversations are preserved.\n'
}

# Execute only after the complete function has been downloaded by curl | sh.
main "$@"
