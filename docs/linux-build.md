# Building and running mobd on other Linux distributions

The `.deb` and the tarball are built and tested on Ubuntu 22.04/24.04 and Debian 12/13 (amd64,
arm64). On other distributions (Fedora, RHEL and its rebuilds, Arch, openSUSE, …) build mobd from
source: it links the system's GStreamer, so the binary matches your glibc. These steps are not
tested in CI; [`mobd desktops doctor`](#4-check) tells you what is still missing.

## 1. Requirements

- **systemd** with **cgroup v2** (the unified hierarchy, default on every current release) and a
  per-user manager (`user@.service`). Each desktop is a transient scope that systemd freezes
  while it sleeps; systemd 246 or newer freezes through D-Bus, older ones through
  `cgroup.freeze`.
- **Kernel 5.7+** (processes start straight in their cgroup, `CLONE_INTO_CGROUP`).
- A user account that runs mobd. Nothing runs as root.

## 2. Packages

| Purpose | Debian/Ubuntu | Fedora / RHEL (EPEL) | Arch | openSUSE |
| --- | --- | --- | --- | --- |
| Build | `golang` 1.27+ (or go.dev), `gcc`, `pkg-config`, `libgstreamer1.0-dev`, `libgstreamer-plugins-base1.0-dev` | `golang`, `gcc`, `pkgconf`, `gstreamer1-devel`, `gstreamer1-plugins-base-devel` | `go`, `gcc`, `pkgconf`, `gstreamer`, `gst-plugins-base` | `go`, `gcc`, `pkg-config`, `gstreamer-devel`, `gstreamer-plugins-base-devel` |
| X server | `xvfb` | `xorg-x11-server-Xvfb` | `xorg-server-xvfb` | `xorg-x11-server-Xvfb` |
| Window manager | `openbox` | `openbox` (EPEL on RHEL) | `openbox` | `openbox` |
| Session bus | `dbus-daemon` | `dbus-daemon` | `dbus` | `dbus-1` |
| Keyboard | `x11-xkb-utils`, `xkb-data` | `setxkbmap`, `xkeyboard-config` | `xorg-setxkbmap`, `xkeyboard-config` | `setxkbmap`, `xkeyboard-config` |
| Encoder | `gstreamer1.0-plugins-bad` (OpenH264, VA-API) | `gstreamer1-plugins-bad-free`, `openh264` and `gstreamer1-plugin-openh264` (Cisco repository), or VA-API | `gst-plugins-bad`, `gst-plugin-openh264` | `gstreamer-plugins-bad`, `gstreamer-plugin-openh264` |
| Fonts | `fonts-dejavu-core`, `fonts-noto-cjk` | `dejavu-sans-fonts`, `google-noto-sans-cjk-fonts` | `ttf-dejavu`, `noto-fonts-cjk` | `dejavu-fonts`, `noto-sans-cjk-fonts` |

A bare Xvfb machine has almost no fonts, so pages render boxes without them. Browsers are optional;
use packaged Chrome, Chromium or Firefox. **Snap and Flatpak browsers do not work:** they cannot
open profiles under `~/.local/share`.

## 3. Build and start

```bash
git clone https://github.com/xrdavies/mobd && cd mobd
make build                       # cgo; fails without the GStreamer headers
install -m 755 bin/mobd ~/.local/bin/mobd
mobd init                        # then edit ~/.mobd/config.json
mobd start                       # writes ~/.config/systemd/user/mobd.service, enables linger
```

`mobd start` runs `loginctl enable-linger` so mobd and its desktops keep running after you log
out. If polkit refuses, run `sudo loginctl enable-linger $USER` once. Without linger the user
manager, mobd and every desktop stop at the last logout.

Built with `CGO_ENABLED=0`, mobd has desktops but no live view (`/health` reports no encoder).

## 4. Check

```bash
mobd status
mobd desktops doctor
```

`doctor` checks Xvfb, openbox, dbus-daemon, setxkbmap, cgroup v2 and the freezer, the encoder,
browsers (and snaps), memory, disk, systemd-oomd, linger and the locale, and prints how to fix
what fails. `/health` shows the same in `checks`.

## 5. Notes per distribution

- **Firewall:** the live view's WebRTC uses UDP `streamPort` (9527).
  `sudo firewall-cmd --add-port=9527/udp --permanent && sudo firewall-cmd --reload`, or
  `sudo ufw allow 9527/udp`. Without it clients fall back to the WebSocket stream.
- **SELinux** (Fedora, RHEL): mobd runs unconfined as your user; no policy changes are known to
  be needed *(to verify)*.
- **systemd-oomd** (Fedora, Ubuntu desktops) may kill a whole desktop scope under memory
  pressure. `doctor` warns when it runs; swap or zram helps more than anything else.
- **Locale:** desktops and agents get `LANG`/`LC_*` from `/etc/default/locale`, else
  `/etc/locale.conf` (Fedora, Arch), else `C.UTF-8`.
- **Uninstall:** `mobd uninstall` closes every desktop and removes the service; delete `~/.mobd`
  and `~/.local/share/mob` to remove all data.
