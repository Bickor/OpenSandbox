---
title: Contributing
description: Guide for contributing to the OpenSandbox project.
---

# Contributing to OpenSandbox

The contributing guide is maintained in the repository root to keep GitHub's contributor experience consistent.

<a href="https://github.com/opensandbox-group/OpenSandbox/blob/main/CONTRIBUTING.md" target="_blank">
  Read the Contributing Guide on GitHub →
</a>

## Contributor Advancement

OpenSandbox maintains an explicit advancement process for contributors to become component maintainers or project maintainers. For nomination requirements, voting periods, and approval thresholds, see the project governance guide:

<a href="https://github.com/opensandbox-group/OpenSandbox/blob/main/GOVERNANCE.md#becoming-a-maintainer" target="_blank">
  Read the Maintainer Advancement Process on GitHub →
</a>

## Fork pull request checks

In `Bickor/OpenSandbox`, test workflows accept pull requests targeting any branch,
including `aks-dev` and feature branches. The required checks are Server CI,
Kubernetes CI, Kubernetes Mini E2E CI, Execd CI, Egress CI, Ingress CI,
Node Agent CI, SDK CI, and Real E2E CI. Each summary verifies that change detection
succeeded and that every relevant test job passed. Unrelated component suites
are explicitly skipped; a failed or cancelled relevant job blocks merging.

The fork uses GitHub-hosted runners for PR tests. Execd smoke tests cover Linux
and Windows; the upstream self-hosted Linux 5.10 kernel leg requires a dedicated
runner and is not part of this fork's matrix. The component workflows can also
be run manually to test all their jobs without path filtering.

Repository merge rules require the CI summaries from GitHub Actions and an
up-to-date PR branch. Older target branches must receive these workflow changes
before their PRs can satisfy the required checks.
