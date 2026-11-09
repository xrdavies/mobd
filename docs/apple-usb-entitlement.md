# Apple USB entitlement application

Status: USB-2. Request submitted 2026-11-10 as FB25504649 (see [Submit](#submit)); waiting for Apple.
Linux import works and is documented for users (mymob.dev/usb/); macOS import waits for this grant.
Companion to the [shared USB redirection design](usb-redirection.md). Until it is granted, the
receiver can be developed on a test Mac with the checks lowered: [macos-usb-development.md](macos-usb-development.md).

## Required capability and owner

**mobd must obtain Apple's managed entitlement before shipping a macOS USB
importer based on `IOUSBHostControllerInterface`:**

```text
com.apple.developer.usb.host-controller-interface
```

This permits the receiving Mac to present a network-backed USB device through a
virtual USB host controller. It belongs on the actual macOS importer executable
or helper and its authorized provisioning profile. Signing Mob Desktop with the
same team's certificate does not grant that capability to mobd.

The development team owns the application; end users install the properly signed
component and grant any required local permissions. They do not each apply to
Apple for the team's entitlement.

| Direction | Application implication |
| --- | --- |
| Local Mac exports to Linux/Windows mobd | This virtual-controller entitlement is not the export permission; investigate capture/driver ownership separately |
| Any supported exporter sends to macOS mobd | The selected virtual-controller import path requires this entitlement |

Developer ID establishes signing identity, notarization checks distribution,
and a provisioning profile authorizes restricted capabilities. Existing Developer
ID certificates and notarization do not automatically grant this entitlement.
Do not confuse it with App Sandbox USB access, DriverKit USB transport, or the
capture permissions discussed by libusb; none is a universal substitute.

## Entry points and official material

| Purpose | Link |
| --- | --- |
| Submit the specific USB request | [Feedback Assistant](https://feedbackassistant.apple.com/) |
| Apple DTS instructions for this exact entitlement | [USB host-controller entitlement discussion](https://developer.apple.com/forums/thread/802495) |
| Developer account identifiers/profiles | [Certificates, Identifiers & Profiles](https://developer.apple.com/account/resources/identifiers/list) |
| General managed-capability process | [Request access to managed capabilities](https://developer.apple.com/help/account/capabilities/capability-requests/) |
| How profiles authorize signed code | [TN3125: Inside Code Signing: Provisioning Profiles](https://developer.apple.com/documentation/technotes/tn3125-inside-code-signing-provisioning-profiles) |
| Developer ID signing and profiles | [Developer ID certificates](https://developer.apple.com/help/account/certificates/create-developer-id-certificates/) |
| Separate DriverKit entitlement process, if an exporter needs it | [System Extensions and DriverKit](https://developer.apple.com/system-extensions/) |

The general capability-request page is background. Apple's specific DTS guidance
directs this USB request to Feedback Assistant because it is not a normal
self-service Xcode capability. Check the linked thread for changes when submitting.

## Submit

Apple DTS (Kevin Elliott, [thread 802495](https://developer.apple.com/forums/thread/802495), checked
2026-11-06): the request is a Feedback Assistant report, not the Capability Requests or DriverKit
forms, and it "cannot be acted on" without the team ID, a product overview and marketing links.
What matters is *what product you are building and why it needs to simulate a USB controller*.

1. **Signed in as the team's Account Holder or an Admin**, open
   [Feedback Assistant](https://feedbackassistant.apple.com/) (or the Feedback Assistant app on a Mac).
2. **New Feedback** → **macOS**.
3. **Title:** `Request for Entitlement - com.apple.developer.usb.host-controller-interface`
4. **Problem area:** USB.
5. **Type of feedback:** Other Bug. In 2026 it was sometimes missing from the menu; then choose
   **Incorrect/Unexpected Behavior** (a request filed that way was still processed), and keep the
   title as above so it is routed as an entitlement request.
6. **Describe the issue:** paste the [request text](#request-text) below.
7. Attach nothing else: no certificates, keys or account details beyond the team ID.
8. Submit, and record the Feedback ID (FB…) and date in the [tracking table](#packaging-gate-and-tracking).
9. Follow up in the report when Apple asks. If nothing moves for about two weeks, reply in
   [thread 802495](https://developer.apple.com/forums/thread/802495) with only the FB number:
   DTS offered there to check routing. Approvals are usually quick but slow down around Apple
   holidays and announcement weeks.

Optional, before or after submitting: register the receiver's App ID in
[Certificates, Identifiers & Profiles](https://developer.apple.com/account/resources/identifiers/list)
(Identifiers → **+** → App IDs → App, macOS, Explicit Bundle ID `com.xrdavies.mobd.usb`). Apple does
not require it for the request; the entitlement is added to that identifier's profiles once granted.

### After approval

1. Apple adds the entitlement to the team. Create **Developer ID** and **Mac Development**
   provisioning profiles for `com.xrdavies.mobd.usb` that include it ([TN3125](https://developer.apple.com/documentation/technotes/tn3125-inside-code-signing-provisioning-profiles)).
2. Build the receiver as a small app-like bundle (a bare CLI cannot carry a profile): the profile as
   `Contents/embedded.provisionprofile`, the entitlement in its signature, signed with the Developer
   ID Application identity and notarized. mobd launches it and hands it the tunnel's socket.
3. Check `codesign -d --entitlements - …`, then import a device on a clean Mac with SIP enabled.

## Request text

Paste as is; Apple asks for exactly these items.

```text
Team ID: 69GK3C9666

Product: Mob (https://mymob.dev/), an open-source (MIT) app for running AI coding agents
(Codex, Claude Code) on a user's own office computer and steering them from a laptop or phone.
The office computer runs our daemon, mobd; the user runs Mob Desktop on their laptop. Both are
distributed outside the Mac App Store, signed with Developer ID and notarized.

What we are building: USB device sharing. A developer with a debug probe or development board
plugged into their laptop shares it, from Mob Desktop, with their paired office computer, so that
the native tools there (and the agents working there) can flash and debug the board as if it
were plugged in locally. It is released for Linux office computers, where mobd attaches the
device through the kernel's USB/IP virtual host controller (vhci_hcd). User guide:
https://mymob.dev/usb/

Why it needs IOUSBHostControllerInterface: when the office computer is a Mac, the device has to
appear to macOS as a real USB device, so that its drivers and existing tools (OpenOCD, dfu-util,
vendor flashing tools) work unchanged. There is no other supported way for a process to present
a remote USB device to macOS; the IOUSBHostControllerInterface header describes exactly this use
("to provide access to remote USB devices").

How it works and its limits:
- The user explicitly picks one device on their laptop and one paired office computer; nothing is
  shared automatically and mobd only ever requests that one device.
- Traffic is USB/IP (the Linux kernel's protocol) inside an authenticated WebSocket tunnel per
  device (TLS where the connection is remote); nothing listens for USB on the network.
- Stopping, quitting the app, a lost connection or revoking the laptop's pairing detaches the
  device at once. Isochronous devices (audio, cameras) and hubs are not supported.

Component that needs the entitlement: a small helper app bundle launched by mobd on the receiving
Mac. Proposed bundle ID: com.xrdavies.mobd.usb (macOS, Apple Silicon).

Please confirm that the entitlement can be used with both Mac Development and Developer ID
provisioning profiles for this bundle ID, and whether any packaging requirement applies to a
helper launched by a command-line daemon.

Links:
- Product: https://mymob.dev/
- USB guide: https://mymob.dev/usb/
- Daemon source: https://github.com/xrdavies/mobd
- Desktop app source: https://github.com/xrdavies/mob
- Design: https://github.com/xrdavies/mobd/blob/main/docs/usb-redirection.md
```

## Packaging gate and tracking

The current [release process](releases.md) signs and installs a standalone mobd
executable. It has no configured USB entitlement/profile-bearing helper bundle.
Do not assume a profile embedded in Mob.app, or an entitlement added to the bare
CLI, authorizes this interface. Prove the helper/app bundle layout, installation,
background launch, update and removal path before advertising macOS import.
Do not modify today's release signing configuration merely to declare the key.

| Item | Current status |
| --- | --- |
| Application owner | mobd maintainers / developer-account owner |
| Team ID | `69GK3C9666` (the Team ID the published installer, dl.mymob.dev/mobd/install.sh, checks mobd's signature against) |
| Importer Bundle ID and packaging | Proposed `com.xrdavies.mobd.usb`, helper app bundle launched by mobd; not built yet |
| Submission date / Feedback ID | 2026-11-10, FB25504649 (Feedback Assistant) |
| Apple approval and distribution profile | Waiting for Apple; release gate remains open |
| Clean-machine device import test | Not performed |

Approval is not guaranteed and does not establish device compatibility. macOS
import remains unavailable until both authorization and real-device acceptance
pass; Linux import does not depend on this Apple application.
