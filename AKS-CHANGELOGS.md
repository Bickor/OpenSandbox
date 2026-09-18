# AKS Changelogs

This file tracks AKS-specific changes carried by the `aks-dev` branch relative to `opensandbox-group/main`. Keep entries in reverse chronological order and include a pull request or commit reference plus a one-sentence description.

## Release process

1. Add new AKS-local changes under `Unreleased` as PRs merge into `aks-dev`.
2. When cutting an AKS dev snapshot, choose a tag in the form `aksdev/YYYYMMDD` (for example, `aksdev/20260726`). If multiple snapshots are cut on the same day, append a numeric suffix such as `aksdev/20260726-1`.
3. Move the relevant `Unreleased` entries into a new section named for that tag.
4. Merge the changelog update, then create and push the tag from the desired `aks-dev` commit:

   ```bash
   git fetch origin
   git checkout aks-dev
   git pull --ff-only origin aks-dev
   git tag -a aksdev/20260726 -m "AKS dev 20260726"
   git push origin aksdev/20260726
   ```

5. Publish AKS images with the same AKS tag. Docker image tags cannot contain `/`, so image publishing sanitizes `aksdev/20260726` to `aksdev-20260726` when composing image tags.

## Unreleased

- _No unreleased AKS-local changes._

## aksdev/20260918-1

- Fixed AKS image version discovery after the upstream Helm charts moved from `kubernetes/charts/` to `manifests/charts/`.

## aksdev/20260918

- [`bdfcd01`](https://github.com/opensandbox-group/OpenSandbox/commit/bdfcd0191e84d9125dec2ccafa9f248fbaf31fe0) — Synced `upstream/main` through the OpenSandbox 1.1.0-rc.1 release, including FastSandbox/Fastlet integration, template-backed sandbox creation, shared egress support, and updated SDKs.

## aksdev/20260812

- [#26](https://github.com/bahe-msft/OpenSandbox/pull/26) — Added typed egress credential plugins with a built-in rotating projected ServiceAccount token provider, request-scoped injection, and operator-controlled provider arguments.

## aksdev/20260811

- [opensandbox-group#1431](https://github.com/opensandbox-group/OpenSandbox/pull/1431) — Synced the reusable image-committer interface, preserved config-digest compatibility, and kept source-registry transport secure by default after AKS Kata/devmapper validation. **Merged upstream.**

## aksdev/20260803

- [#24](https://github.com/bahe-msft/OpenSandbox/pull/24) — Recovered missing source-image content by digest before snapshot commits, including authenticated ACR recovery with Workload Identity. **Core source-image recovery merged upstream:** [opensandbox-group#1431](https://github.com/opensandbox-group/OpenSandbox/pull/1431); authenticated ACR recovery remains AKS-local.
- [#23](https://github.com/bahe-msft/OpenSandbox/pull/23) — Fixed AKS ingress image publishing after the Kubernetes dependency update raised the required `golang.org/x/time` version.

## aksdev/20260802-1

- [#21](https://github.com/bahe-msft/OpenSandbox/pull/21) — Added a containerd-native image committer and an Azure variant that pushes snapshots to ACR with AKS Workload Identity. **Core pluggable image-committer interface merged upstream:** [opensandbox-group#1431](https://github.com/opensandbox-group/OpenSandbox/pull/1431); the Azure ACR variant remains AKS-local.

## aksdev/20260802

- [#19](https://github.com/bahe-msft/OpenSandbox/pull/19) — Replaced the Agent Sandbox internal endpoint fix from [#18](https://github.com/bahe-msft/OpenSandbox/pull/18) with the implementation from [opensandbox-group#1424](https://github.com/opensandbox-group/OpenSandbox/pull/1424), including IPv6 Pod endpoint formatting while preserving BatchSandbox endpoint resolution.

## aksdev/20260728

- [#14](https://github.com/bahe-msft/OpenSandbox/pull/14) — Renewed DNS-derived nftables entries while TCP connections remain active to preserve reconnect access without globally extending stale IP authorization. **Merged upstream:** [opensandbox-group#1399](https://github.com/opensandbox-group/OpenSandbox/pull/1399).
- [#15](https://github.com/bahe-msft/OpenSandbox/pull/15) — Upgraded mitmproxy to 11.0.2 to use upstream HTTP/2 flow control and restore fast Bazel Remote Execution downloads. **Merged upstream:** [opensandbox-group#1396](https://github.com/opensandbox-group/OpenSandbox/pull/1396).

## aksdev/20260726-1

- [`f6f70ad`](https://github.com/bahe-msft/OpenSandbox/commit/f6f70add8bc7111c41b9f75ead3ee5dbd10e7396) — Cross-compiled Go binaries before Docker packaging to avoid slow arm64 Go builds under QEMU.
- [`cdffbfe`](https://github.com/bahe-msft/OpenSandbox/commit/cdffbfe2787c4b9911cad72f8ea594b244dbc2b6) — Skipped code-interpreter and removed the Windows execd binary from AKS image publishing to reduce publish time.
- [#11](https://github.com/bahe-msft/OpenSandbox/pull/11) / [`2fc47c2`](https://github.com/bahe-msft/OpenSandbox/commit/2fc47c266df6fbb76a9ec3776a026a8c33065822) — Added a dedicated GHCR workflow for publishing AKS images with AKS tag-aware image tags.
- [#8](https://github.com/bahe-msft/OpenSandbox/pull/8) / [`a53e0f7`](https://github.com/bahe-msft/OpenSandbox/commit/a53e0f766bb728174df486f4272201a909e8f00e) — Added a PTY shell fallback so execd can start interactive sessions when `bash` is unavailable. **Merged upstream:** [opensandbox-group#1359](https://github.com/opensandbox-group/OpenSandbox/pull/1359).
- [#7](https://github.com/bahe-msft/OpenSandbox/pull/7) / [`0f16ab8`](https://github.com/bahe-msft/OpenSandbox/commit/0f16ab85c5e6356ab8a3d313db546af665cc4375) — Synced the Kata guest filesystem before snapshot commits to improve image-committer consistency. **Merged upstream:** [opensandbox-group#1356](https://github.com/opensandbox-group/OpenSandbox/pull/1356).
