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
        printf 'Install mobd for macOS Apple Silicon or Linux (x86_64, arm64).\nUsage: sh install.sh [--version X.Y.Z] [--install-dir /absolute/path]\n'
        return ;;
      *) printf 'Unknown option: %s\n' "$1" >&2; return 1 ;;
    esac
  done
  [ "$(id -u)" -ne 0 ] || { printf 'Run this installer as your normal user, without sudo.\n' >&2; return 1; }
  case "$(uname -s)" in
    Darwin)
      os=darwin
      [ "$(uname -m)" = arm64 ] || [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ] || {
        printf 'mobd requires an Apple Silicon Mac.\n' >&2; return 1;
      }
      os_major=$(sw_vers -productVersion | cut -d. -f1)
      [ "$os_major" -ge 13 ] || { printf 'mobd requires macOS 13 or newer.\n' >&2; return 1; }
      platform=darwin-aarch64 archive_arch=darwin_arm64 ;;
    Linux)
      os=linux
      case "$(uname -m)" in
        x86_64|amd64) platform=linux-x86_64 archive_arch=linux_amd64 ;;
        aarch64|arm64) platform=linux-aarch64 archive_arch=linux_arm64 ;;
        *) printf 'mobd on Linux supports x86_64 and arm64.\n' >&2; return 1 ;;
      esac ;;
    *) printf 'mobd supports macOS (Apple Silicon) and Linux.\n' >&2; return 1 ;;
  esac
  case "$install_dir" in /*) ;; *) printf 'Installation directory must be an absolute path.\n' >&2; return 1 ;; esac
  if [ "$os" = darwin ]; then
    running() { launchctl print "gui/$(id -u)/com.xrdavies.mobd" >/dev/null 2>&1 || pgrep -u "$(id -u)" -x mobd >/dev/null 2>&1; }
    field() { plutil -extract "$1" raw -o - "$2"; }
    checksum() { shasum -a 256 "$1" | cut -d ' ' -f1; }
  else
    running() { systemctl --user is-active --quiet mobd 2>/dev/null || pgrep -u "$(id -u)" -x mobd >/dev/null 2>&1; }
    # release.json is written by release.mjs with two-space indentation: "version" at the top,
    # each platform's "sha256" on the line after its "url".
    field() {
      case "$1" in
        version) sed -n 's/^  "version": "\([^"]*\)".*/\1/p' "$2" | head -n 1 ;;
        platforms.*.sha256)
          name=${1#platforms.}
          awk -v key="\"${name%.sha256}\"" '
            index($0, key) == 5 { inside = 1; next }
            inside && /"sha256"/ { gsub(/.*"sha256": "|".*/, ""); print; exit }
            inside && /^    }/ { exit }' "$2" ;;
      esac
    }
    checksum() { sha256sum "$1" | cut -d ' ' -f1; }
  fi
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
    requested_version=$(field version "$temp_dir/latest.json")
  fi
  printf '%s\n' "$requested_version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || {
    printf 'Invalid stable version. Expected X.Y.Z.\n' >&2; return 1;
  }
  prefix="$download_base/mobd/v$requested_version"
  archive="mobd_${requested_version}_${archive_arch}.tar.gz"
  fetch "$prefix/release.json" "$temp_dir/release.json"
  [ "$(field version "$temp_dir/release.json")" = "$requested_version" ] || { printf 'Release version mismatch.\n' >&2; return 1; }
  expected=$(field "platforms.$platform.sha256" "$temp_dir/release.json")
  [ -n "$expected" ] || [ "$os" = darwin ] || {
    printf 'mobd %s has no Linux build yet. Linux releases are coming soon.\n' "$requested_version" >&2; return 1;
  }
  printf '%s\n' "$expected" | grep -Eq '^[0-9a-f]{64}$' || { printf 'Invalid release checksum.\n' >&2; return 1; }
  printf 'Downloading mobd %s…\n' "$requested_version"
  fetch "$prefix/$archive" "$temp_dir/$archive"
  actual=$(checksum "$temp_dir/$archive")
  [ "$actual" = "$expected" ] || { printf 'Checksum mismatch. Installation stopped.\n' >&2; return 1; }
  # Only regular files with these exact names may be extracted into the temporary directory.
  tar -tzf "$temp_dir/$archive" > "$temp_dir/contents"
  [ "$(LC_ALL=C sort "$temp_dir/contents")" = "$(printf 'LICENSE\nREADME.md\nmobd')" ] || { printf 'Unexpected archive contents.\n' >&2; return 1; }
  tar -tvzf "$temp_dir/$archive" | awk 'substr($0,1,1) != "-" {bad=1} END {exit bad}' || { printf 'Archive contains links or special files.\n' >&2; return 1; }
  tar -xzf "$temp_dir/$archive" -C "$temp_dir" mobd
  [ "$os" = linux ] || codesign --verify --strict --verbose=2 -R "=anchor apple generic and identifier \"com.xrdavies.mobd\" and certificate leaf[subject.OU] = \"$apple_team\" and certificate leaf[field.1.2.840.113635.100.6.1.13] exists" "$temp_dir/mobd"
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
  if [ "$os" = linux ]; then
    printf '\nConversation desktops need (Debian, Ubuntu):\n  sudo apt install xvfb openbox dbus-daemon x11-xkb-utils gstreamer1.0-plugins-base gstreamer1.0-plugins-bad fonts-dejavu-core\n'
    printf 'Or install the .deb from the release, which brings them. Check with: mobd desktops doctor\n'
    printf 'Sound in the live view also needs: sudo apt install pulseaudio gstreamer1.0-pulseaudio\n'
  fi
}

# Execute only after the complete function has been downloaded by curl | sh.
main "$@"
