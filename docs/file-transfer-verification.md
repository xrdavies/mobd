# File transfer verification

Verified on 2026-10-29, on `feat/file-transfers` in mobd, mob, mob-ios, mob-android and mob-site.
Protocol and scope: [file transfers](file-transfers.md).

## Implementations

| Component | Library / behavior |
| --- | --- |
| mobd | tusd 2.10.1, disk storage and file locks; SHA-256 publication; HTTP Range downloads |
| Desktop | tus-js-client 4.3.1; Tauri native file dialogs and disk staging |
| iOS | TUSKit 3.7.1, 4 MiB foreground chunks; document import/export |
| Android | tus-java-client 0.5.1, 4 MiB requests; Storage Access Framework |
| Website | `/files/` guide, documentation navigation, search and sitemap |

## Results

| Check | Result |
| --- | --- |
| mobd `go test ./...` | Passed |
| mobd file API tests with `-race` | Passed |
| Desktop `npm test` with integration fixture | 96 passed |
| Desktop `npm run build` | Passed |
| Desktop Rust library tests on macOS | 5 passed |
| iOS simulator `mob-iosTests` | 73 executed, 7 unrelated opt-in tests skipped, 0 failures |
| Android JVM tests | 64 passed, including the real-server transfer case |
| Android API 36 emulator, `FileTransferDeviceTest` | Passed |
| Android debug APK build | Passed |
| Website tests / build | 7 passed / 14 pages built |

The SDK checks use an isolated local mobd with temporary files and a temporary paired
credential. They exercise real HTTP upload and download requests, pause both directions,
restore the client/store, continue, and verify bytes. Test data includes 9 MiB and 17 MiB
binary files, Unicode names and empty files. Android also checks duplicate-name publication
and completion receipt recovery. Desktop's store integration replaces the Tauri disk bridge
with a temporary-file bridge; the native disk implementation is checked separately in Rust.

Server tests additionally cover reconstruction of the file service, stale offsets, partial
file invisibility, repeated completion, cancellation, mismatched checksums, changed-file
validators, unsafe filenames, symlinks, authentication, CORS and Java's PATCH method override.

The first interoperability runs found and fixed two SDK integration problems: the desktop
reader must return a Blob with its size, and Java must finish an empty upload without opening
an unnecessary PATCH request.

## Reproduce

Start the isolated fixture from mobd:

```sh
MOB_FILE_TEST_FIXTURE=/tmp/mob-files-fixture.json \
  go test ./internal/api -run '^TestFileTransferIntegrationServer$' -count=1 -timeout=65m -v
```

The JSON contains `url`, `token` and the test process `pid`, is written with mode 0600, and
is removed on shutdown. It is a disposable test credential, never a production pairing.
Stop the fixture with SIGINT/SIGTERM to its recorded PID. Its temporary data is removed.

* Desktop: set `MOB_FILE_TEST_FIXTURE` to that JSON and run `npm test`.
* Android JVM: set the same variable and run `./gradlew :app:testDebugUnitTest`.
* iOS: pass its `url` and `token` as `TEST_RUNNER_MOB_FILE_TEST_URL` and
  `TEST_RUNNER_MOB_FILE_TEST_TOKEN` to `xcodebuild test -only-testing:mob-iosTests`.
* Android emulator: pass `fileTestUrl` and `fileTestToken` as instrumentation arguments
  to `FileTransferDeviceTest`; replace the fixture URL's host with `10.0.2.2`.

Use the existing Xcode project/scheme and a simulator destination, and Android's
`-Pandroid.testInstrumentationRunnerArguments.NAME=VALUE` convention. Do not put tokens
in committed configuration or documents.

## Remaining acceptance

Real phones, third-party file providers, manual system save-dialog flows, Windows/Linux
desktop builds, 20 GiB files, disk exhaustion and throttled/off-Wi-Fi gateway paths were
not tested. Client reconstruction is tested; OS process termination during every disk write
is not. Continuous background transfer is deliberately outside this release.

The optional full iOS UI suite stalled in an existing accessibility-test teardown and was
stopped. The `mob-iosTests` target, including the file transfer integration, then passed
separately. This is not a claim of full UI acceptance.

Paused/abandoned uploads retain server disk space until canceled. iOS staging/export can
temporarily require another full copy; leave sufficient free space. Agent prompt changes,
remote deletion and automatic expiry remain outside this implementation.
