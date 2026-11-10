# mobd for Windows x64

This portable ZIP provides the current Windows build for manual testing. It is not
Authenticode-signed. Verify the ZIP against the release's SHA256SUMS before extracting.

Extract to a new folder and run from PowerShell as your normal user:

```powershell
.\mobd.exe --version
.\mobd.exe init
notepad "$env:LOCALAPPDATA\mobd\config.json"
.\mobd.exe serve
```

Run `init` only for a first installation; it never overwrites an existing config.
Set `workdir` to an existing absolute directory and `runners` to installed runners.
Use doubled backslashes or forward slashes in JSON paths. Runner authentication is
managed separately by each runner. Launching npm `.cmd` shims is not yet verified.

The initial configuration listens on all IPv4 interfaces at port 8080. For local
testing, set `listen` to `127.0.0.1:8080` before starting. For another device, choose
an appropriate LAN/VPN address and allow the connection in Windows Firewall.
In another terminal, `mobd.exe link` creates the client pairing invitation.

Keep the foreground terminal open. There is no Windows service manager yet:
`start`, `stop`, `restart`, `status` and `logs` are not supported. Stop the foreground
process with Ctrl+C. Screenshots, desktop input and Live View are not implemented
on Windows. `mobd remote setup` is also not supported on Windows.

For an upgrade, finish active tasks and stop the foreground process, then extract
the new ZIP into a new folder. Keep `%LOCALAPPDATA%\mobd` and any custom data paths.
No installer, PATH change, firewall rule or login startup is applied by this ZIP.

See https://github.com/xrdavies/mobd/blob/main/docs/windows.md for implementation status.
