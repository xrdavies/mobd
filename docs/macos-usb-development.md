# Developing the macOS USB receiver without Apple's entitlement

**Development only.** Not an installation step for users, and never needed for a Linux office
computer. Companion to the [USB design](usb-redirection.md) and the
[Apple entitlement runbook](apple-usb-entitlement.md).

A Mac office computer receives a shared USB device through Apple's virtual USB host controller
(`IOUSBHostControllerInterface`), which needs the managed entitlement
`com.apple.developer.usb.host-controller-interface` with a provisioning profile from Apple. Until
Apple grants it, the receiver can only run on a test Mac whose security checks are lowered:

1. **System Integrity Protection (SIP) off**, which also lets the boot arguments below be set.
2. **`amfi_get_out_of_my_way=1`**: AMFI then runs code that claims an entitlement without a
   matching profile. SIP off alone does not do that.

Both weaken the whole Mac, not just mobd: unsigned code and kernel-level changes are no longer
stopped, and on Apple Silicon the Mac drops to Permissive Security (iOS apps and Apple Pay stop
working). Do it on a Mac kept for testing, keep the time short, and turn both back on afterwards.
Screen Recording and Accessibility grants for the dev-signed mobd ([dev signing](dev-signing.md))
are not expected to change, but check them after each restart.

Steps for Apple Silicon (M1 and later). You need physical access: recoveryOS cannot be reached
over SSH or Screen Sharing, so have a display and keyboard on the Mac.

## Record the starting state

In Terminal, before changing anything:

```bash
csrutil status          # expect: enabled
nvram boot-args         # expect: "data was not found" (none set)
```

Note any existing boot arguments: you will restore them, not delete them, at the end.
M4002 on 2026-11-06: SIP enabled, no boot arguments.

## Turn the checks off

1. Shut down the Mac (Apple menu → Shut Down). Wait until it is fully off.
2. Press and hold the power button until **Loading startup options** appears, then release.
3. Choose **Options** → **Continue**. Select an administrator account and enter its password.
4. In the menu bar, choose **Utilities** → **Terminal**, then run:

   ```bash
   csrutil disable
   ```

   Answer `y`, then enter the administrator's name and password when asked. It reports that
   SIP is disabled after a restart.
5. Restart from the Apple menu → **Restart**, back into macOS.
6. In Terminal:

   ```bash
   sudo nvram boot-args="amfi_get_out_of_my_way=1"
   ```

   If there were boot arguments before, put them in the same quoted string, separated by spaces.
7. Restart again.
8. Check:

   ```bash
   csrutil status       # System Integrity Protection status: disabled.
   nvram boot-args      # boot-args	amfi_get_out_of_my_way=1
   ```

9. Start mobd again from a login shell, as usual on M4002
   (`zsh -lc "cd ~/Projects/mob-project/mobd && ./bin/mobd start"`), and check its permissions
   with `mobd status`.

## Turn the checks back on

1. In Terminal, remove the boot argument, or restore the earlier value:

   ```bash
   sudo nvram -d boot-args                    # there were none before
   # or: sudo nvram boot-args="<earlier value>"
   ```

2. Shut down, then hold the power button until **Loading startup options**, and choose
   **Options** → **Continue** and an administrator, as above.
3. **Utilities** → **Terminal**:

   ```bash
   csrutil enable
   ```

   Answer `y` and authenticate.
4. Back in the startup options, check **Utilities** → **Startup Security Utility**: select the
   disk, choose **Security Policy…** and make sure **Full Security** is selected (choose it if
   not, and authenticate).
5. Restart into macOS and check:

   ```bash
   csrutil status       # System Integrity Protection status: enabled.
   nvram boot-args      # "data was not found", or the earlier value
   ```

6. Start mobd from a login shell and check `mobd status` as above. Without the checks lowered,
   `mobd usb status` reports again that macOS needs Apple's entitlement.

## While the checks are off

- Use the Mac only for this work. Do not install or run software you do not need for it.
- Results show what the receiver does, not that it passes Apple's checks: before a release it
  must run with SIP on and a real profile ([release gate](apple-usb-entitlement.md#packaging-gate-and-tracking)).
- Record in [Feasibility results](usb-redirection.md#feasibility-results) which macOS version and
  devices were tested this way.
