# Contributing to Agent Testnet

Thanks for your interest. Agent Testnet is open source under the [GNU Affero General Public License v3.0 or later](LICENSE). The project welcomes contributions from independent developers, researchers, and commercial organizations alike.

This document covers the legal side of contributing. For build, run, and architecture docs see the [README](README.md) and `docs/`.

## License of contributions (inbound = outbound)

By contributing to this repository you agree that your contribution is licensed under the same **AGPL-3.0-or-later** terms as the rest of the project. We do not require a Contributor License Agreement (CLA). We use the **Developer Certificate of Origin** instead — see below.

## The Developer Certificate of Origin (DCO)

Every commit must be signed off, which is your statement that you have the right to submit the patch under the project's license. The DCO is a lightweight, well-understood mechanism used by the Linux kernel, Docker, GitLab, and many other projects. The full text is at <https://developercertificate.org/>; the short version is "I wrote this, or I have the right to submit it, and I'm OK with it being shipped under the project's license".

To sign off a commit, append a `Signed-off-by` trailer using your real name and a reachable email:

```bash
git commit -s -m "Your commit message"
```

This produces a commit message ending with:

```
Signed-off-by: Jane Doe <jane@example.com>
```

If you forget, you can amend the most recent commit with `git commit --amend -s`, or for older commits rebase with `git rebase --signoff <base>`.

## Pull requests

- Keep PRs focused. One logical change per PR makes review and revert easy.
- Run `make test` and `make smoke` before opening a PR where applicable.
- New user-facing behavior should come with a docs update under `docs/` or in the relevant README section.
- Breaking changes to APIs, on-disk formats, or wire formats need an explicit note in the PR description.

## Reporting security issues

Please do not file public issues for security vulnerabilities. Instead, contact the maintainers privately (see the repo's security policy when published) so a fix can be coordinated before disclosure.

## A note on the project name

The AGPL license covers the *code*. The name "Agent Testnet" and the project's logo are how the community identifies the canonical project. Forks are welcome to use the code under AGPL but should not present themselves as "Agent Testnet" or imply endorsement by this project. A more detailed trademark policy will follow.
