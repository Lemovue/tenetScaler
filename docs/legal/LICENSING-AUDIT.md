# tenetScaler Licensing Audit

**Audit date:** 2026-11-12 · **Scope:** Lemovue/tenetScaler, all branches and history · **Status:** working document for the four Founders. It is not legal advice.

> All four founders should read this audit. LICENSE section 7 and the agreements refer to section 6 for third-party obligations.

This audit does not establish who owns anything. Git history shows who committed code, not who wrote it, and not who owns the copyright. Ownership moves to the collective structure only when the agreements in this folder are signed (section 10).

## 1. Method

| Area | How it was checked |
|---|---|
| Repository and GitHub settings | `git` on a fresh clone; read-only `gh api` calls (repository, branches, rulesets, organization members, teams, collaborators, PRs). No setting was changed. |
| Contributors | `git shortlog` on `main` and all branches; GitHub API commit-to-login mapping; `git blame -w -M -C` over every file on `main` except vendored third-party files; PR and merge records. |
| Go dependencies | `go list -deps ./cmd/tenantscaler` for linux/amd64, windows/amd64 and darwin/arm64; license and NOTICE files read from the module cache; `go mod verify`; copyleft scan of all license files and linked sources. |
| Other third-party material | Vendored files, fonts, download scripts, Dockerfile and dataset citations in the code, checked against their sources where reachable. |
| Copyright notices | `git grep` for copyright, SPDX and license text across all branches. |

## 2. Summary

1. **No license file existed** before this change. The repository has been public since 2026-10-27 with no license. It has 0 forks, 0 releases and 0 tags.
2. **Four founders.** The four git author identities and the four GitHub organization owners are the same four people, as stated by one founder on 2026-11-12. Each founder confirms their own account in writing at signature (Schedule 3 of the founders agreement). No other author appears in git history.
3. **Commit authorship is not reliable evidence of who wrote what** (section 4). Each founder must document their own contributions before ownership shares are agreed.
4. **Third-party licenses are all permissive** (Apache-2.0, MIT, BSD-3-Clause, ISC, OFL-1.1). They are compatible with a proprietary license, but **compiled binaries and container images currently ship none of the required notices** (section 6).
5. **Nothing is protected on GitHub yet.** There is no branch protection, no ruleset, no CI and no team. All four founders are organization owners. Organization-wide 2FA is not required.

## 3. Identified original contributors

Numbered in alphabetical order of GitHub username. The order implies no ranking.

| Founder | GitHub username | Git author name (as recorded in git, not a verified legal name) | Commits on `main`* | PRs authored | Surviving lines on `main`** |
|---|---|---|---|---|---|
| Founder 1 | akshatdalakoti-stack | Akshat Dalakoti | 5 | #3 (`hades`) | 26,609 |
| Founder 2 | Harshcodes04 | Harshcodes04 | 14 | #2 (`gateway`) | 16,794 |
| Founder 3 | mayankdev-oss | Mayank Joshi | 5 | #1 (`mayank`) | 8,724 |
| Founder 4 | nilay007hckkr | Nilay Joshi | 23 | #4, #5, #6 (`nilay`) | 5,039 |

\* Includes merge commits made in the GitHub web interface: Harshcodes04 merged #1, #3 and #4; nilay007hckkr merged #2, #5 and #6. The merges added no content.
\*\* `git blame -w -M -C` at `main` (1d2bd52), 57,166 lines, excluding three.js and the Bangers font. **These counts measure who committed lines, not who wrote them. They are not ownership shares.**

Email addresses are recorded in git history and are not repeated here.

### Areas by committing author on `main`

| Area | Mostly committed by |
|---|---|
| `internal/arbiter`, `config`, `control`, `controller`, `estimate`, `forecast`, `model`, `policy`, `stabilize`; `config/real.yaml`; `scripts/get-trace.ps1` | mayankdev-oss |
| `internal/fairq`, `gateway`, `loadgen`, `metrics`, `web` (own code); most of `internal/demo`; `test/e2e`; `scripts/build-dist.*`, `scripts/run-demo.*` | Harshcodes04 |
| `internal/autoscale`, `engine`, `eval`, `llama`, `pool`, `realrun`, `replica`, `scalerun`, `sim`, `stack`, `topology`; most of `internal/k8s`, `workload`, `experiment`; `deploy/k8s`; `Dockerfile`; `scripts/build.*`, `lint.*`, `test.*`, `setup-llama.ps1` and `scripts/dev/` | akshatdalakoti-stack |
| `README.md`, `DEMO.md`, `docs/`, `internal/envfile`, `deploy/k3s`, `scripts/k3s`, `scripts/demo/`, `go.mod`, root config files; parts of `internal/demo`, `workload`, `k8s`, `cmd/tenantscaler` | nilay007hckkr |

`cmd/tenantscaler` is split among all four.

## 4. Authorship evidence the founders must resolve

These facts come from the repository. What they mean for ownership is for the founders and their lawyer to decide.

- **Bulk import.** The 20 feature-code commits on the `mayank` (5), `gateway` (10) and `hades` (5) branches were all made within about 11 minutes on 2026-10-27 (21:13 to 21:24 IST), on top of a skeleton commit made at 21:10. Some are seconds apart. The five `hades` commits add about 84,000 lines in 7 seconds, about 57,000 of them generated experiment data under `results/` (later removed from `main`). None of the three feature branches builds on its own, because each imports packages that exist only on the others. The first commit's `go.mod` already listed dependencies used only by later code.
- **An earlier repository.** Code and documents refer to commit IDs that do not exist in this repository. Removed planning files describe an earlier local repository with no remote.
- **Material outside git.** `docs/BRIEF-v2.md` names `framework.txt` as a source of truth, but that file was never committed. Removed team documents refer to PDFs not in the repository. Both may contain founder work that needs to be listed.
- **Edits across authors.** One commit (e187229) changed fixture dates in 12 files committed by other founders. Two commits reached `main` without a PR: the initial skeleton commit 99931f9, and 1d2bd52, which deleted 212 files committed by two other founders (204 generated `results/` files and 8 planning documents) and edited `.gitignore`. All six PRs were merged without review.

**Consequence:** the Founder Contribution Record (Schedule 3 of the founders agreement) must describe what each founder actually created: research, design, code, review, documents, data and experiment runs. It cannot simply copy the table in section 3.

## 5. Existing copyright notices

- The project's own files had no copyright header, SPDX line or license notice on any branch. No LICENSE, NOTICE, AUTHORS or CONTRIBUTORS file was ever committed.
- The only notices belong to third parties: three.js (`@license`, MIT, in `three.module.min.js` and `LICENSE-three.txt`) and the Bangers font (`OFL-Bangers.txt`). They must be kept.
- Per-file headers are optional. The new LICENSE covers the repository. Adding headers would touch every source file, so it is not done here.

## 6. Third-party licensing obligations

### 6.1 Material in the repository

| Material | Location | License | Obligation |
|---|---|---|---|
| three.js r170 | `internal/web/static/vendor/three.module.min.js` | MIT | Keep `LICENSE-three.txt` and the `@license` header. It is embedded in the binary through `go:embed`. |
| Bangers font | `internal/web/static/fonts/Bangers-Regular.ttf` | SIL OFL 1.1, no Reserved Font Name | Keep `OFL-Bangers.txt` with it. It may be bundled with proprietary software but not sold on its own. |

### 6.2 Go modules compiled into the binary

53 modules are linked into `./cmd/tenantscaler` (52 on Windows and macOS, where `prometheus/procfs` is not linked). All `go.mod` requirements are linked. A further 39 modules are in the module graph (`go list -m all`) but are not compiled in; 11 of them have entries in `go.sum`.

| License | Modules (examples) |
|---|---|
| Apache-2.0 | `k8s.io/{api,apimachinery,client-go,klog/v2,kube-openapi,utils}`, `prometheus/*`, `go-openapi/*`, `go-logr/logr`, `gnostic-models`, `modern-go/*`, `sigs.k8s.io/{json,randfill,structured-merge-diff/v6}`, parts of `sigs.k8s.io/yaml`, `go.yaml.in/yaml/{v2,v3}` |
| MIT | `beorn7/perks`, `cespare/xxhash/v2`, `emicklei/go-restful/v3`, `fxamacker/cbor/v2`, `json-iterator/go`, `x448/float16`, parts of `sigs.k8s.io/yaml`, `go.yaml.in/yaml`, one file inside `prometheus/client_golang` |
| BSD-3-Clause | `golang.org/x/{net,oauth2,sys,term,text,time}`, `google.golang.org/protobuf`, `google/uuid`, `spf13/pflag`, `munnerz/goautoneg`, `gopkg.in/{evanphx/json-patch.v4,inf.v0}`, parts of `sigs.k8s.io/yaml` (`fields.go`), the Go standard library and runtime |
| ISC | `davecgh/go-spew` |

No GPL, LGPL, AGPL, MPL, EPL or CDDL code was found in the license files or in the linked source files. All of these licenses allow use in proprietary software.

**Obligations:**

- **Source only (the repository as it is):** no Go module code is redistributed, so no module license text has to ship. Keep the three.js and Bangers notices.
- **Binaries and container images** (`scripts/build-dist.*` and the `Dockerfile`): the license text of every linked module, one copy of the Apache-2.0 license, the Go toolchain license and the attribution content of 9 NOTICE files must go with each binary or image. The 9 NOTICE files belong to `go-openapi/jsonpointer`, `go-openapi/jsonreference`, `prometheus/client_golang`, `client_model`, `common`, `procfs`, `go.yaml.in/yaml/v2`, `v3` and `sigs.k8s.io/randfill`. **Nothing does this today.** Do not distribute a binary or image outside the founders until a third-party notices file exists and the build ships it. A tool such as `go-licenses` can generate the file. Add the MIT header of `client_golang`'s `prometheus/internal/almost_equal.go` by hand, because it has no separate license file.
- **Container base image** `gcr.io/distroless/static:nonroot`: the distroless tooling is Apache-2.0. The image also ships Debian 13 packages under their own licenses, among them `base-files` (GPL-2+), `ca-certificates` (GPL-2+ and MPL-2.0), `netbase` (GPL-2) and `tzdata` (public domain). Anyone distributing the image must meet those packages' terms. The tag is not pinned by digest, so the contents change over time. The `golang:1.27` build stage does not ship.

### 6.3 Downloaded by scripts, not in the repository

None of these were ever committed (checked with `git rev-list --objects --all`). Licenses were read on each source's main branch, not at the pinned version.

| Material | Fetched by | License | Notes |
|---|---|---|---|
| llama.cpp release b11898 | `scripts/setup-llama.ps1` | MIT | The script also downloads NVIDIA's `cudart` runtime zip. NVIDIA's own terms apply to it (not verified). Do not redistribute it. |
| Qwen2.5-0.5B-Instruct GGUF | `scripts/setup-llama.ps1` | Apache-2.0 | Not redistributed. |
| Azure LLM inference trace 2024 | `scripts/get-trace.ps1` | CC-BY-4.0 (data); the dataset repository's code is MIT | The code cites "CC-BY" without the version or a link. If trace-derived data or charts are published, add "CC BY 4.0", the license link and a note that the data was modified. Trace-derived data cannot be placed under the proprietary LICENSE. |
| BurstGPT trace | User-supplied path (`internal/workload`) | CC-BY-4.0 | Same as above. The test fixtures appear hand-written, not copied. |
| k3s, KEDA | Installed by operators following `deploy/k3s/README.md` | Apache-2.0 | Referenced only |

Results committed under `results/` (removed from `main`, still in history) are the project's own measurements, not copies of the traces.

### 6.4 Dashboard code

The dashboard JavaScript, CSS and HTML contain no external URLs except the SVG namespace, no CDN references, no embedded images and no third-party credits. Two files include the widely circulated `mulberry32` random-number snippet (`decor.js`, `spider3d.js`), a few lines long, with a comment naming it. Its license was not verified. `internal/web/THEME_NOTES.md` says nothing was copied from the visual references, and no copied text was found. A scan cannot prove originality.

### 6.5 Published methods

The founders state that the system's design is based on their own manual study of published research papers. The scheduler and forecaster implement published techniques: virtual-token-counter fair queuing and Holt-Winters forecasting. Copyright does not protect methods and ideas as such, so using them creates no copyright license obligation. Patents were not searched [VERIFY WITH COUNSEL]. A manual review found no code copied from a publication; this does not prove originality. If any publication's own code was used, list it as third-party material. [ADD CITATIONS OF THE RESEARCH PAPERS THE DESIGN IS BASED ON]

### 6.6 Development-only tools

Python (`psutil`), Node.js and PowerShell scripts under `scripts/` and `internal/web/` are used only during development. Nothing from them is redistributed.

## 7. Contributions requiring legal assignment

All original material in the repository was committed by the four founders. All of it needs to be covered by the signed founders agreement:

- everything on `main` except the third-party material in section 6.1;
- material on other branches and in history: `results/` data (204 files), `PLAN.md`, `CLAIMS.md`, `phase_1_.md` and `docs/team/` (removed from `main` in 1d2bd52 but still on `origin/nilay` and `origin/nilay-backup-2026-11-02`), and the unmerged commit 786061f on `origin/nilay-backup-2026-11-02`;
- founder material outside git (section 4): `framework.txt`, the PDFs named in the removed team documents, the earlier local repository, and any research and design work;
- the README's "Visual inspiration" credit: no copied asset, image or code was found, but the founder who made the dashboard should confirm that nothing was copied.

There are no external contributors today. Any future contributor signs `CONTRIBUTOR-IP-ASSIGNMENT-AGREEMENT.md` first.

## 8. Missing founder information

| Missing item | Where needed |
|---|---|
| Legal names of all four founders | every agreement |
| Postal addresses and notice emails | founders agreement, use authorization |
| Ownership percentages (equal only if all four confirm) | founders agreement, Schedule 1 |
| Each founder's Background IP | founders agreement, Schedule 2 |
| Each founder's contribution record | founders agreement, Schedule 3 |
| Third-party claims: university, employer, sponsor, scholarship | founders agreement, Schedule 4 |
| Licensing contact email `[LICENSING CONTACT EMAIL]` | LICENSE, README, CONTRIBUTING, agreements |
| Security contact email `[SECURITY CONTACT EMAIL]` | CONTRIBUTING |
| Governing law, dispute forum, arbitration rules | every agreement |
| Approval threshold for non-commercial authorizations (default: all four) | founders agreement, use authorization. If it is lowered, LICENSE section 5 must change too. |
| Exit, death, incapacity and valuation terms | founders agreement |

## 9. Remaining legal risks

1. **Nothing has been signed.** Until all four founders sign the founders agreement, each founder keeps whatever rights they have under the default law. In some jurisdictions a co-owner may license without the others' consent [VERIFY WITH COUNSEL].
2. **University IP policy.** At least one founder's git identity uses a university email domain, and the project may be academic work. No university IP policy for student work was found online. Ask the university's research or IPR office in writing, and check any course, capstone, sponsor or competition terms the founders accepted, before claiming exclusive ownership [VERIFY WITH COUNSEL].
3. **Public history.** Files removed from `main` (`PLAN.md`, `CLAIMS.md`, `phase_1_.md`, `docs/team/`, `results/`) are still public through the `nilay` and `nilay-backup-2026-11-02` branches and the commit history. Some contain local file paths and internal notes. Deleting branches or rewriting history is irreversible and is not done here. The founders should decide on it together.
4. **Rights already granted.** Under GitHub's Terms of Service (section D.5), making a repository public grants every GitHub user a license to use, display, perform and fork it through GitHub's functionality. That cannot be withdrawn while the repository is public, and the new LICENSE does not try to. Section D.6 licenses contributions under the repository's license only once a license notice exists. That is a license, not an assignment, so a signed contributor agreement is still needed.
5. **Assignment formalities.** If Indian law governs, an assignment must be in writing and signed, and must state the work, the rights, the duration, the territory and the consideration. If the duration is missing it defaults to five years, if the territory is missing it defaults to India, and rights not exercised within one year lapse unless the deed says otherwise (Copyright Act 1957, s.19). Authors keep the rights of attribution and integrity after assignment (s.57) [VERIFY WITH COUNSEL]. Fill in every placeholder in the agreements.
6. **Unprotected binaries** (section 6.2): distributing them without notices would breach the dependency licenses.
7. **Governance depends on trust.** All four founders are organization owners. Any one of them can still change visibility, delete or transfer the repository, or edit protection rules. Once the founders agreement is signed, its no-unilateral-action clause will govern this; until then, only the founders' mutual trust and the audit log guard against it.
8. **Deadlock.** Unanimous approval means any one founder can block a decision, and four founders can split two against two. The founders agreement sends both to dispute resolution (clauses 12.7, 12.8 and 13).
9. **Lemovue is not a legal entity.** Licenses are signed, and fees received, by the four founders personally. Tax and liability consequences need advice.
10. **Placeholders in a live LICENSE.** `[LICENSING CONTACT EMAIL]` must be filled in before the LICENSE is merged. Otherwise nobody can request authorization.

## 10. Outstanding approvals and signatures

- [ ] All four founders approve the LICENSE text and the move to a proprietary model (Unanimous Founder Approval).
- [ ] All four sign `FOUNDERS-IP-OWNERSHIP-AGREEMENT.md` after completing Schedules 1 to 4 and a lawyer has reviewed it.
- [ ] All four approve the templates `CONTRIBUTOR-IP-ASSIGNMENT-AGREEMENT.md` and `PROJECT-USE-AUTHORIZATION.md`.
- [ ] A founder applies the GitHub settings in `GITHUB-BRANCH-PROTECTION-CHECKLIST.md` and another founder verifies them.

## 11. Recommended actions before enforcing the new licensing policy

1. Fill in the licensing and security contact emails. Use an address that all four founders can access.
2. Each founder completes their Schedule 3 contribution record and Schedule 4 disclosures, including any university, employer or sponsor claim.
3. Check the university IP policy and any course, sponsor or competition rules (section 9, item 2).
4. Agree ownership percentages, governing law, approval thresholds and exit terms. Have a lawyer review all agreements.
5. All four sign the founders agreement. Record the signature dates privately.
6. Review this audit together.
7. Merge the LICENSE, README, CONTRIBUTING, CODEOWNERS and `docs/legal/` changes through a PR approved by the founders.
8. Create the `founders` team. Confirm every member has 2FA on, then require 2FA. Apply the branch protection checklist.
9. Before distributing any binary or image, add a third-party notices file and ship it with the build (section 6.2).
10. Decide what to do with the stale public branches (section 9, item 3).
11. Only after steps 1 to 8 are done, announce the licensing policy.
