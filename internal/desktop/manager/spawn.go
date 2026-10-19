//go:build linux

package manager

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// xvfbArgs: a 24-bit screen of the desktop's pixel size, local connections only, the cookie
// required, no screen saver, and no reset when the last client leaves.
func xvfbArgs(n, width, height, scale int, xauth string) []string {
	return []string{
		":" + strconv.Itoa(n),
		"-screen", "0", fmt.Sprintf("%dx%dx24", width*scale, height*scale),
		"-dpi", strconv.Itoa(96 * scale),
		"-nolisten", "tcp",
		"-auth", xauth,
		"-s", "0",
		"-noreset",
		"+extension", "RANDR",
	}
}

// dbusConfig is a session bus at a fixed address, so the record and turns know it in advance.
func dbusConfig(socket string) []byte {
	return []byte(`<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <keep_umask/>
  <listen>unix:path=` + escape(socket) + `</listen>
  <auth>EXTERNAL</auth>
  <standard_session_servicedirs/>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
`)
}

// openboxRC is openbox's configuration, regenerated at every start: one workspace (so an agent
// cannot lose windows on another), click to focus and raise, title bars to move windows, and no
// key bindings, so every key an agent or viewer sends reaches the application.
var openboxRC = []byte(`<?xml version="1.0" encoding="UTF-8"?>
<openbox_config xmlns="http://openbox.org/3.4/rc">
  <desktops><number>1</number></desktops>
  <focus><focusNew>yes</focusNew><followMouse>no</followMouse></focus>
  <placement><policy>Smart</policy><center>yes</center></placement>
  <theme><name>Clearlooks</name><titleLayout>NLIMC</titleLayout></theme>
  <keyboard></keyboard>
  <mouse>
    <dragThreshold>3</dragThreshold>
    <context name="Frame">
      <mousebind button="Left" action="Press"><action name="Focus"/><action name="Raise"/></mousebind>
    </context>
    <context name="Client">
      <mousebind button="Left" action="Press"><action name="Focus"/><action name="Raise"/></mousebind>
      <mousebind button="Middle" action="Press"><action name="Focus"/><action name="Raise"/></mousebind>
      <mousebind button="Right" action="Press"><action name="Focus"/><action name="Raise"/></mousebind>
    </context>
    <context name="Titlebar">
      <mousebind button="Left" action="Drag"><action name="Move"/></mousebind>
      <mousebind button="Left" action="DoubleClick"><action name="ToggleMaximize"/></mousebind>
    </context>
    <context name="Top"><mousebind button="Left" action="Drag"><action name="Resize"><edge>top</edge></action></mousebind></context>
    <context name="Bottom"><mousebind button="Left" action="Drag"><action name="Resize"><edge>bottom</edge></action></mousebind></context>
    <context name="Left"><mousebind button="Left" action="Drag"><action name="Resize"><edge>left</edge></action></mousebind></context>
    <context name="Right"><mousebind button="Left" action="Drag"><action name="Resize"><edge>right</edge></action></mousebind></context>
    <context name="BLCorner"><mousebind button="Left" action="Drag"><action name="Resize"/></mousebind></context>
    <context name="BRCorner"><mousebind button="Left" action="Drag"><action name="Resize"/></mousebind></context>
    <context name="TLCorner"><mousebind button="Left" action="Drag"><action name="Resize"/></mousebind></context>
    <context name="TRCorner"><mousebind button="Left" action="Drag"><action name="Resize"/></mousebind></context>
    <context name="Close"><mousebind button="Left" action="Click"><action name="Close"/></mousebind></context>
    <context name="Maximize"><mousebind button="Left" action="Click"><action name="ToggleMaximize"/></mousebind></context>
    <context name="Iconify"><mousebind button="Left" action="Click"><action name="Iconify"/></mousebind></context>
  </mouse>
</openbox_config>
`)

func escape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// childEnv is mobd's environment without another display or bus, plus the desktop's.
func childEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "DISPLAY", "XAUTHORITY", "DBUS_SESSION_BUS_ADDRESS", "WAYLAND_DISPLAY":
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}
