# GitHub Branch Protection and Repository Safety Checklist

**Manual checklist - nothing here has been applied. Do not remove any Founder's admin access.**

This is an operational checklist for the four Founders of Lemovue. It is not an agreement and not legal advice. It changes nothing by itself: an organization owner must apply each item by hand in the GitHub UI, after the Founders have agreed on it. GitHub settings are technical controls only. A pull request approval, comment or settings change on GitHub is not a "Founder Approval" under `docs/legal/FOUNDERS-IP-OWNERSHIP-AGREEMENT.md`. That requires a document signed by each required Founder.

**The Founders.** The four GitHub accounts that appear in the git history and as the four organization owners are the four Founders, to be confirmed in writing when the Founders agreement is signed. Listed alphabetically by username (the order implies no ranking): Founder 1 `akshatdalakoti-stack`, Founder 2 `Harshcodes04`, Founder 3 `mayankdev-oss`, Founder 4 `nilay007hckkr`. Their legal names are recorded in the agreement as [FOUNDER n LEGAL NAME]; git display names are not verified legal names. "Unanimous Founder Approval" means the written approval of all four Founders. Because all four owners are Founders, once the agreement is signed every owner-level action will be governed by its No Unilateral Action clause.

**Accounts are not ownership.** Git history and GitHub admin rights do not by themselves establish copyright ownership. Each Founder documents their actual contributions under the agreement. Surviving-line counts from `git blame` are not a measure of contribution or ownership share.

**Disagreement.** If the Founders cannot agree on an item, including a 2-2 split or one Founder withholding approval, leave the setting as it is and follow the agreement's deadlock and dispute-resolution clauses. No Founder has a casting vote unless the Founders add one by unanimous amendment.

Lemovue is not a legal entity; references to Lemovue are to the GitHub organization and project identity only [UPDATE IF A LEGAL ENTITY IS FORMED AND IP IS ASSIGNED TO IT].

Nothing here purports to restrict the rights GitHub's Terms of Service give GitHub users over public content (viewing, and forking within GitHub's functionality), and nothing here grants more.

## 0. Current state (checked read-only on 2026-11-12)

| Item | State | Read-only source |
|---|---|---|
| Repository `Lemovue/tenetScaler` | public, default branch `main`, forking allowed | `gh api repos/Lemovue/tenetScaler` |
| License detected by GitHub | none (`license: null`) | same |
| Branch protection on `main` | none (404 "Branch not protected") | `.../branches/main/protection` |
| Repository rulesets | none; no rules apply to `main` | `.../rulesets`, `.../rules/branches/main` |
| Other branches | 5 besides `main`, none protected | `.../branches` |
| Tags | none | `.../tags` |
| GitHub Actions workflows | 0, so there are no status checks to require | `.../actions/workflows` |
| `.github/CODEOWNERS` on `main` | not present (`codeowners/errors` returns 404) | `.../codeowners/errors` |
| Automatically delete head branches | off | `delete_branch_on_merge` |
| Secret scanning / push protection | both `disabled` | `security_and_analysis` |
| Organization plan | Free | `gh api orgs/Lemovue` |
| Organization teams | none (`[]`) | `orgs/Lemovue/teams` |
| Organization members | 4 members, all 4 are owners | `orgs/Lemovue/members?role=admin` |
| Repository collaborators | the same 4 accounts, all `admin`; 0 outside collaborators | `.../collaborators` |
| Git history | 4 distinct author identities | `git log --format='%an' \| sort -u` |
| `two_factor_requirement_enabled` | `false` (member 2FA status is checked privately, not recorded here) | `orgs/Lemovue` |
| `members_can_delete_repositories` | `true` | `orgs/Lemovue` |
| `members_can_change_repo_visibility` | `true` | `orgs/Lemovue` |
| `members_allowed_repository_creation_type` | `all` | `orgs/Lemovue` |
| `web_commit_signoff_required` | `false` (organization and repository) | `orgs/Lemovue`, repository |
| Organization rulesets | not visible with current token (needs `admin:org`) | `orgs/Lemovue/rulesets` |

Re-run section 6 before you start: if any row has changed, update this table first.

## 1. Prerequisites

- [ ] **1.1 Confirm the Founder accounts.** All four GitHub accounts that appear in the git history and as organization owners are the four Founders: `akshatdalakoti-stack`, `Harshcodes04`, `mayankdev-oss` and `nilay007hckkr`. Confirm this in writing when `docs/legal/FOUNDERS-IP-OWNERSHIP-AGREEMENT.md` is signed, and check that the same four usernames appear there and in `.github/CODEOWNERS`. Why: the team and `.github/CODEOWNERS` must name exactly the Founders.
- [ ] **1.2 Every member has 2FA on.** Each founder turns on 2FA for their own account. Run `gh api 'orgs/Lemovue/members?filter=2fa_disabled' --jq length` (read-only); it must return 0 before you do 1.3.
- [ ] **1.3 Require two-factor authentication.** Only after 1.2 returns 0: Organization **Settings** > Security: **Authentication security** > "Require two-factor authentication for everyone in your organization" > **Save**. Why: an owner account without 2FA is the easiest way to lose control of the repository. Per GitHub's docs, members without 2FA keep their membership but cannot access organization resources until they enable it, so doing 1.2 first keeps every Founder's access. Outside collaborators without 2FA are removed (there are none today).
- [ ] **1.4 Create the team `founders`.** Organization > **Teams** > **New team**. Name: `founders`, so the slug matches `@Lemovue/founders` in `.github/CODEOWNERS`. Under "Team visibility" choose visible (a CODEOWNERS team must be visible). Add exactly the four Founder accounts (`akshatdalakoti-stack`, `Harshcodes04`, `mayankdev-oss`, `nilay007hckkr`), and nobody else.
- [ ] **1.5 Give the team Write or higher on the repository.** Repository **Settings** > Access: **Collaborators & teams** > **Add teams** > `founders` > role **Write** (or higher). Why: GitHub skips a CODEOWNERS team without write permission on the repository, even when its members are admins.
- [ ] **1.6 Merge `.github/CODEOWNERS` into `main` through a pull request.** Then open the file on GitHub and check that no errors are highlighted. Why: review requests use the CODEOWNERS file on the pull request's base branch.

## 2. Protect `main`

Prefer a repository ruleset. Path: repository **Settings** > "Code, planning, and automation": **Rules** > **Rulesets** > **New ruleset** > **New branch ruleset** (label may differ; verify in UI).

- [ ] **2.1 Name and status.** Ruleset name: `main-protection`. Enforcement status: **Active**.
- [ ] **2.2 Target.** "Add a target" > include the default branch (label may differ; verify in UI). Why: it keeps covering `main` if the default branch is ever renamed.
- [ ] **2.3 Bypass list: the Founders decide this explicitly.** Recommended: leave it empty, so the rules apply to every account, the four owners included. Removing bypass does not remove anyone's admin privileges: owners can still edit or disable the ruleset, and that change appears in the audit log. If a bypass is ever agreed, prefer "For pull requests only" to "Always allow".
- [ ] **2.4 Restrict deletions** (on by default). Why: nobody can delete `main`.
- [ ] **2.5 Block force pushes** (on by default). Why: git history records which account committed what and when (it does not by itself show who wrote it or who owns it; see "Accounts are not ownership" above). A force push to `main` can rewrite it. Note: while this rule is on, owners cannot rename the default branch unless they can bypass the ruleset.
- [ ] **2.6 Require a pull request before merging**, with these sub-options:
  - [ ] Required approvals: at least **1** (label may differ; verify in UI).
  - [ ] **Dismiss stale pull request approvals when new commits are pushed**.
  - [ ] Require review from Code Owners (label may differ; verify in UI). Turn this on only after 1.4 to 1.6 are done. Note: an approval from any one member of `founders` satisfies it.
  - [ ] **Require approval of the most recent reviewable push**. Why: the last person to push cannot approve their own final change.
  - [ ] Require conversation resolution before merging (label may differ; verify in UI).
- [ ] **2.7 Decide on extra review for `LICENSE` and `docs/legal/**`.** Options:
  - (a) If the UI offers it, the ruleset's **Required reviewers** option: team `founders`, file patterns `LICENSE` and `docs/legal/**`, 2 approvals. It was not confirmed that this option is available on the Free plan.
  - (b) A second ruleset or a written policy that such pull requests are merged only after two Founders approve.
  - (c) Set 2 required approvals for every pull request.

  Whichever you choose, a GitHub approval does not replace the signed Unanimous Founder Approval needed to change `LICENSE` or the licensing model.
- [ ] **2.8 Save the ruleset** (**Create**), then run the checks in section 6.

**Alternative: classic branch protection.** Repository **Settings** > **Branches** > **Add classic branch protection rule**. Branch name pattern: `main`. Select **Require a pull request before merging**, **Require approvals** (1), **Dismiss stale pull request approvals when new commits are pushed**, **Require review from Code Owners** (after 1.6), **Require approval of the most recent reviewable push**, **Require conversation resolution before merging** and **Do not allow bypassing the above settings**. Leave **Allow force pushes** and **Allow deletions** unchecked. Why the bypass box matters: by default, classic rules do not apply to admins. If you use both a ruleset and a classic rule, all rules apply together and the most restrictive version wins. Use one method unless you want them layered.

Consequence: with no bypass and at least one approval, every change to `main` needs a second person. If only one Founder is available, merges wait. That is intended.

## 3. Status checks (none exist today)

- [ ] **3.1 Add CI as a separate, reviewed pull request.** This checklist does not create it. A workflow such as `.github/workflows/ci.yml` should run `scripts/lint.sh` and `scripts/test.sh`, or their steps (`gofmt -l`, `go vet ./...`, `go test` on the unit-test packages). Things to know:
  - `go.mod` requires Go 1.27.0.
  - `lint.sh` also needs `node` and `python3`.
  - `test.sh` also runs `test/e2e`; its real-replica test skips unless `TS_LLAMA_SERVER` and `TS_LLAMA_MODEL` are set.
  - Do not have CI download the llama.cpp binaries, the GGUF model or the traces unless the Founders agree. They are third-party materials under their own licenses (`docs/legal/LICENSING-AUDIT.md`).
- [ ] **3.2 Require the checks.** After the workflow has run at least once on a pull request, edit `main-protection`: **Require status checks to pass before merging** > add [REQUIRED STATUS CHECK NAMES], and select **Require branches to be up to date before merging**. Per GitHub's docs, the up-to-date option has no effect until a check is defined.

## 4. Repository deletion, transfer and visibility

- [ ] **4.1** Organization **Settings** > Access: **Member privileges** > "Repository deletion and transfer": clear "Allow members to delete or transfer repositories for this organization" > **Save**.
- [ ] **4.2** Same page > "Repository visibility change": clear "Allow members to change repository visibilities for this organization" > **Save**.
- [ ] **4.3 Understand the limit.** Member privileges restrict members, not owners. All four Founders are owners today, so 4.1 and 4.2 change nothing in practice until someone's role changes. Any owner can still delete, transfer or change the visibility of the repository (**Settings** > "Danger Zone"). Once `docs/legal/FOUNDERS-IP-OWNERSHIP-AGREEMENT.md` is signed, each of those actions will need Unanimous Founder Approval under its No Unilateral Action clause. Until then, only the founders' mutual trust and the audit log (5.6) guard against them.
- [ ] **4.4 Owner count: decide, do not act alone.** All four owners are the Founders, with identical roles. Do not remove, downgrade or add any owner without Unanimous Founder Approval under the agreement; that approval includes the Founder whose role would change. If the owner set ever changes, all four Founders keep identical roles.
- [ ] **4.5 Keep the repository public.** Changing visibility needs Unanimous Founder Approval. Making it private would not withdraw copies: per GitHub's docs, public forks are detached and stay public.
- [ ] **4.6 Optional:** limit repository creation to owners. Organization **Settings** > **Member privileges** > repository creation (label may differ; verify in UI). This matters only for non-owner members.

## 5. Other recommended settings

- [ ] **5.1 Protect release tags.** **Rulesets** > **New ruleset** > **New tag ruleset**, named `release-tags`, set to **Active**. Target all tags or a pattern such as `v*`. Enable **Restrict updates**, **Restrict deletions** and **Block force pushes**. **Restrict creations** is optional: if it is on, only bypass actors (for example the `founders` team) can create matching tags. Why: a published release tag should not move or disappear silently. There are no tags today.
- [ ] **5.2 Optional:** a branch ruleset targeting all branches with only **Block force pushes**, to keep history on the other 5 branches. Do not add **Restrict deletions** to all branches if you use 5.3, because then merged head branches could not be deleted automatically.
- [ ] **5.3 Optional:** repository **Settings** > **General** > "Pull Requests" > **Automatically delete head branches**.
- [ ] **5.4 Secret scanning and push protection.** Repository **Settings** > "Security and quality": **Advanced Security** > to the right of "Secret Protection" click **Enable**, then to the right of "Push protection" click **Enable**. Why: the repository is public, and a pushed token or API key is exposed at once. Both are disabled today.
- [ ] **5.5 Forking (information only, no action).** GitHub's **Allow forking** setting applies to private repositories owned by an organization. A public repository can be forked by GitHub users under GitHub's Terms of Service. Do not try to restrict this.
- [ ] **5.6 Review the audit log** every [AUDIT LOG REVIEW INTERVAL]. Organization **Settings** > "Archive": **Logs** > **Audit log**. Look for repository deletion, transfer and visibility changes, owner or role changes, team changes, and ruleset or branch-protection changes. Raise anything unexpected with all four Founders.
- [ ] **5.7 Optional:** require sign-off on web-based commits (organization or repository commit signoff setting; label may differ; verify in UI). A sign-off is not a contributor agreement. Because of GitHub's "inbound = outbound" rule and because `LICENSE` grants almost nothing, accept no external contribution until its author has signed `docs/legal/CONTRIBUTOR-IP-ASSIGNMENT-AGREEMENT.md` (see `CONTRIBUTING.md`).

## 6. Verification (read-only)

Every command below is a GET request. Do not add `-X`, `--method`, `-f` or `-F`, because those make `gh api` send changes. Run each one after a Founder applies the matching item. Each comment gives the expected result.

```sh
R=repos/Lemovue/tenetScaler
gh api $R --jq '{visibility, default_branch, delete_branch_on_merge}'          # public, main (4.5, 5.3)
gh api $R --jq '.security_and_analysis | {s: .secret_scanning.status, p: .secret_scanning_push_protection.status}'  # enabled, enabled (5.4)
gh api orgs/Lemovue --jq '{two_factor_requirement_enabled, members_can_delete_repositories, members_can_change_repo_visibility}'  # true, false, false (1.3, 4.1, 4.2)
gh api 'orgs/Lemovue/members?filter=2fa_disabled' --jq length                  # 0 (1.2, 1.3)
gh api 'orgs/Lemovue/members?role=admin' --jq '[.[].login]'                    # the four Founders, unless changed by Unanimous Founder Approval (4.4)
gh api orgs/Lemovue/teams/founders --jq '{slug, privacy}'                      # founders, closed (= visible) (1.4)
gh api orgs/Lemovue/teams/founders/members --jq '[.[].login]'                  # exactly the four usernames in 1.1, any order (1.4)
gh api $R/teams --jq '.[] | {slug, permission}'                                # founders: push, maintain or admin (1.5)
gh api $R/codeowners/errors                                                    # {"errors":[]} (1.6)
gh api $R/rulesets --jq '.[] | {id, name, target, enforcement}'                # main-protection, release-tags: active
gh api $R/rulesets/<ID> --jq '{bypass_actors, inc: .conditions.ref_name.include, rules: [.rules[].type]}'
  # main-protection: bypass_actors [] (2.3), inc ["~DEFAULT_BRANCH"], rules incl. deletion, non_fast_forward, pull_request
gh api $R/rulesets/<ID> --jq '.rules[] | select(.type=="pull_request") | .parameters'
  # required_approving_review_count >= 1, dismiss_stale_reviews_on_push, require_code_owner_review,
  # require_last_push_approval, required_review_thread_resolution all true (2.6)
gh api $R/rules/branches/main --jq '[.[].type] | unique'                       # all rules in force on main, from all rulesets
gh api $R/branches/main/protection                                             # only if classic protection was used instead
gh api $R/actions/workflows --jq '.workflows[] | {name, path, state}'          # the CI workflow, once added (3.1)
```

`bypass_actors` is visible only to accounts that can edit the repository. The audit log (5.6) is checked in the UI.

## 7. Record of application

Fill in one row per section once it has been applied and verified. A different Founder should verify than the one who applied it.

| Section | Applied by (GitHub username) | Date | Verified by (GitHub username) |
|---|---|---|---|
| 1 | | | |
| 2 | | | |
| 3 | | | |
| 4 | | | |
| 5 | | | |

## Sources (GitHub Docs, read on 2026-11-12)

- About rulesets: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/about-rulesets
- Available rules for rulesets: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets
- Creating rulesets for a repository: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/creating-rulesets-for-a-repository
- About protected branches: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/about-protected-branches
- Managing a branch protection rule: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-protected-branches/managing-a-branch-protection-rule
- About code owners: https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners
- Deleting or transferring repositories: https://docs.github.com/en/organizations/managing-organization-settings/setting-permissions-for-deleting-or-transferring-repositories
- Repository visibility changes: https://docs.github.com/en/organizations/managing-organization-settings/restricting-repository-visibility-changes-in-your-organization
- Requiring 2FA: https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-two-factor-authentication-for-your-organization/requiring-two-factor-authentication-in-your-organization
- Creating a team: https://docs.github.com/en/organizations/organizing-members-into-teams/creating-a-team
- Team and people access: https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/managing-repository-settings/managing-teams-and-people-with-access-to-your-repository
- Push protection: https://docs.github.com/en/code-security/how-tos/secure-your-secrets/prevent-future-leaks/enable-push-protection
- Automatic deletion of branches: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-the-automatic-deletion-of-branches
- Organization audit log: https://docs.github.com/en/organizations/keeping-your-organization-secure/managing-security-settings-for-your-organization/reviewing-the-audit-log-for-your-organization
- Repository visibility: https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/managing-repository-settings/setting-repository-visibility
- Forking policy: https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/managing-repository-settings/managing-the-forking-policy-for-your-repository
- Commit signoff policy: https://docs.github.com/en/organizations/managing-organization-settings/managing-the-commit-signoff-policy-for-your-organization
