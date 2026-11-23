# Windows x64 CCS runtime

The optional Windows guest tool runs CCS 8.11.0 with Node 24.22.2. Its lockfile overrides CCS's undici dependency to 6.29.1. The application entry point stays at `node_modules/@kaitranntt/ccs/dist/ccs.js`.

Run the following commands from this directory on the packaging host. The first command populates the host npm cache from the lockfile. The packer installs from that cache into a fresh temporary directory with lifecycle scripts disabled. It requires Python 3.11 or newer.

```sh
npm ci --ignore-scripts --os=win32 --cpu=x64 --include=optional --no-bin-links --no-audit --no-fund
uv run --offline --no-project python pack.py /path/to/ccs-8.11.0-win-x64.tar
```

The uncompressed tar contains sorted regular files with fixed metadata and a SHA-256 manifest. Compare the two digests printed by the packer with `$archiveSha` and `$manifestSha` in `../install-ccs.ps1`. The guest bridge also pins the manifest digest in `../CCSRuntime.cs`.

To exercise both validators on the completed archive, run `pwsh -NoProfile -File verify.tests.ps1 /path/to/ccs-8.11.0-win-x64.tar` from a writable directory without symlink ancestors. The test requires PowerShell 7.6 or newer.

Stage the tar and the pinned Node archive with the updated installer and guest bridge. Run the installer as the paired ordinary Windows user, then copy the verified receipt into the protected bridge directory through the existing management process. The patched runtime installs alongside the old runtime and uses the existing CCS profile. An updated bridge requires the new receipt before it can launch CCS.
