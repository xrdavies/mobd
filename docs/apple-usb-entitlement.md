# Apple USB entitlement application

Status: draft USB-1, 2026-11-01. Preparation only; no application was submitted
by this documentation change. Existing account approvals have not been inspected.
Companion to the [shared USB redirection design](usb-redirection.md).

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

## Application steps

1. Have the team's account holder/authorized maintainer confirm the Team ID,
   distribution method and actual importer target identity. The current daemon
   identifier is `com.xrdavies.mobd`; an entitled helper's Bundle ID is still to
   be selected. Do not invent an identifier or infer approval from a certificate.
2. Sign in to Feedback Assistant and start a macOS report. Use the title
   `Request for Entitlement - com.apple.developer.usb.host-controller-interface`
   and the USB problem area. Apple DTS specifies the feedback type **Other Bug**.
   If the current UI does not offer it, use an available appropriate category,
   explicitly label the report an entitlement request and ask Developer Support
   to confirm routing; do not claim a missing field is a technical product bug.
3. Attach the materials below and explain why the receiving Mac must create a
   virtual USB controller. Request confirmation that the grant/profile supports
   both development and the intended Developer ID distribution outside the App Store.
4. Record the Feedback ID and date. Respond to requests for information; if
   routing is unclear, follow up through Apple support/the linked DTS discussion
   with the Feedback ID. Do not post certificates, private keys or account secrets.
5. Once approved, obtain the appropriate profile for the exact target, configure
   its entitlements, embed the profile in the supported bundle layout, sign and
   notarize the resulting distribution. Verify effective entitlements and profile
   authorization, then exercise the USB interface on a clean Mac with SIP enabled.

Submitting the report and following up are external communications and must be
performed as a separately authorized action. Drafting this document does neither.

## Materials to prepare

Apple DTS explicitly asks for Team ID, a product overview and product/company
links. Supply these and the following implementation context:

| Material | Value or preparation |
| --- | --- |
| Team ID and contact | Fill from the verified developer account; no credential/private key attachment |
| Product | Mob: open-source remote agent workspace, with a planned local-to-remote USB sharing feature |
| Public product information | [Mob website](https://mymob.dev/), [Desktop repository](https://github.com/xrdavies/mob), [mobd repository](https://github.com/xrdavies/mobd) |
| Technical design | [USB design](https://github.com/xrdavies/mobd/blob/main/docs/usb-redirection.md); confirm the link resolves before submitting |
| Actual target | Daemon/helper Bundle ID, team, deployment OS/architectures, bundle/launch design; finalize before submission |
| Need for the interface | Remote macOS tools need a virtual USB device backed by hardware attached to the user's separate computer |
| Distribution | Developer ID signed and notarized macOS distribution outside the App Store; explain proposed helper packaging |
| Intended devices | Tested/targeted USB drives, development boards and debug probes; distinguish tested results from planned support |
| Access controls | Explicit local device selection, paired destination, authenticated encrypted transport, exclusive ownership, revocation and cleanup |
| Supporting evidence | Architecture diagram, prototype description or logs if available; label unimplemented behavior accurately |

Suggested request body, to complete and review before submission:

```text
Team ID: <verified Team ID>
Product: Mob / mobd
Importer Bundle ID: <final Bundle ID of the macOS importer component>
Distribution: Developer ID signed and notarized, outside the Mac App Store

Mob is an open-source remote agent workspace. We plan to let a user explicitly
share a USB device attached to their local computer with their paired remote
Mac running mobd. On that receiving Mac, native drivers and development tools
need to access the device through a virtual USB host controller.

We request com.apple.developer.usb.host-controller-interface for the receiving
macOS importer, using IOUSBHostControllerInterface. The planned transport is
USB/IP carried over an authenticated encrypted connection. The design limits
each share to a selected device and destination, provides explicit release,
and terminates access on credential revocation or connection failure.

Please confirm the supported provisioning and packaging requirements for
development and Developer ID distribution of this component.

Product: https://mymob.dev/
Source: https://github.com/xrdavies/mobd
Design: https://github.com/xrdavies/mobd/blob/main/docs/usb-redirection.md
Prototype status and target devices: <accurate current status>
Supported macOS versions/architectures and bundle layout: <verified plan>
Contact: <maintainer contact>
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
| Team and prior account grant | Not checked in this change |
| Importer Bundle ID and packaging | Pending implementation proof |
| Submission date / Feedback ID | No submission by this change |
| Apple approval and distribution profile | Not verified; release gate remains open |
| Clean-machine device import test | Not performed |

Approval is not guaranteed and does not establish device compatibility. macOS
import remains unavailable until both authorization and real-device acceptance
pass; Linux import does not depend on this Apple application.
