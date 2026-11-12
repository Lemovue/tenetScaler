# Contributing to tenetScaler

tenetScaler is proprietary, source-available software (see [LICENSE](LICENSE)). The code is public so it can be inspected, but contributing to it and using it both need written agreements with the four founders. This page explains the process.

Who this page applies to:

- **The four founders** contribute under the founders' IP ownership agreement ([`docs/legal/FOUNDERS-IP-OWNERSHIP-AGREEMENT.md`](docs/legal/FOUNDERS-IP-OWNERSHIP-AGREEMENT.md)). They are not external contributors and do not sign the contributor agreement.
- **Everyone else** ("external contributors") follows the steps below.

> The documents in `docs/legal/` are drafts that have not been signed. Until they are executed, the founders cannot accept external contributions.

## 1. Propose a contribution

1. **Open an issue first.** Describe the problem or the change you have in mind, and wait for a founder to reply before writing code. Changes nobody agreed to may not be accepted.
2. **Sign the contributor agreement** (section 2) before you open a pull request.
3. **Open a pull request from your fork.** Keep it focused on one change, follow the conventions in [`docs/DESIGN.md`](docs/DESIGN.md), and run the checks from the README:

   ```bash
   ./scripts/lint.sh    # PowerShell: ./scripts/lint.ps1
   ./scripts/test.sh    # PowerShell: ./scripts/test.ps1
   ```

   Say in the pull request which checks you ran and their result.
4. **Review.** GitHub requests a review from the `founders` team named in [`.github/CODEOWNERS`](.github/CODEOWNERS). A pull request into `main` needs approval from a founder other than the last person who pushed to it. Only a founder merges.

Never put passwords, API keys, tokens, `.env` contents, personal data or confidential material in an issue, a pull request or a commit.

## 2. Contributor IP agreement

A pull request from an external contributor is merged only after that contributor has signed the contributor IP assignment agreement ([`docs/legal/CONTRIBUTOR-IP-ASSIGNMENT-AGREEMENT.md`](docs/legal/CONTRIBUTOR-IP-ASSIGNMENT-AGREEMENT.md)) and the founders have countersigned it.

- **How:** email tenetscaler-licensing@googlegroups.com with your name and GitHub username. The founders send you the agreement to sign. Do not post signed agreements or identity documents in public issues.
- **Why it is required:** GitHub's Terms of Service license a contribution to a repository under the repository's own license. This repository's LICENSE grants almost no rights, so without a signed agreement the project could not use your contribution.
- **Employer or university:** if an employer, university, sponsor or anyone else may own what you create, get their written permission before you contribute, and say so in the agreement.
- **Moral rights:** any moral rights that the law does not allow you to transfer stay with you.
- **AI-generated content:** say in the pull request which parts of your contribution were generated wholly or substantially by an AI tool, and name the tool (agreement section B6).

Contributing does not give you a license to use the software.

## 3. Authorization to use the software

Installing, building, running, modifying, redistributing or commercially using tenetScaler needs prior written authorization (see LICENSE, sections 4 and 5).

1. Email tenetscaler-licensing@googlegroups.com with who you are (person or organization), what you want to do, whether the use is commercial, how long and where you will use it, and whether you need to modify or redistribute it.
2. The founders decide together. An authorization is valid only once it is written on the form in [`docs/legal/PROJECT-USE-AUTHORIZATION.md`](docs/legal/PROJECT-USE-AUTHORIZATION.md) (or an equivalent document) and signed by all four founders. No single founder can grant it alone.
3. A reply by email or chat, or a comment on GitHub, is not an authorization.

## 4. Third-party code, dependencies and assets

Do not add third-party code, Go modules, fonts, images, datasets, models or other material without disclosing it in the pull request and getting the written approval of all four founders. For each item, give:

| Field | Example |
|---|---|
| Name and version (or commit) | three.js r170 |
| Source URL | where you got it |
| License (SPDX identifier) | MIT |
| How it is used | linked into the binary / vendored file / downloaded by a script / development only |
| Files added or changed | `internal/web/static/vendor/...` |
| Obligations | notice or license text that must ship with it |

Rules:

- `go.mod` and `go.sum` are fixed (see `docs/DESIGN.md`, section 0). A new or upgraded dependency needs the written approval of all four founders.
- A vendored file keeps its license text next to it, as `internal/web/static/vendor/LICENSE-three.txt` does for three.js.
- Code under copyleft licenses (GPL, LGPL, AGPL and similar), code without a clear license, and snippets copied from websites or Q&A sites are not accepted unless all four founders approve them in writing.
- Never remove or edit a third-party license notice.

## 5. Contact

- **Technical questions and bug reports:** a GitHub issue on [Lemovue/tenetScaler](https://github.com/Lemovue/tenetScaler/issues).
- **Licensing, authorization and contributor agreements:** tenetscaler-licensing@googlegroups.com.
- **Security vulnerabilities:** report them privately to tenetscaler-licensing@googlegroups.com, not in a public issue.
- **Maintainers:** the four founders (GitHub team `@Lemovue/founders`).
