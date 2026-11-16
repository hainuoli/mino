# Contributing to Mino

Small, focused changes with clear motivation and tests are especially welcome.

## Direction

Mino follows a direction its maintainer chooses deliberately, not one reached
by consensus. Ideas are welcome, and some will be excellent, but a proposal
may be declined when it does not fit that direction. A declined idea says
nothing about the idea's value or yours.

## Before you start

- Search existing issues and pull requests first.
- Open an issue before changing a public API or adding a package.
- Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## Workflow

1. Fork the repository.
2. Keep one logical change per pull request.
3. Add tests for behavior changes and update the affected documentation.
4. Run the checks below.
5. Open a pull request using the template.

## Commit messages

Mino uses [gitmoji](https://gitmoji.dev) meanings: one emoji, one space, one
imperative sentence. The emoji also sets the label that files the change into
the release notes.

```text
<emoji> <Imperative sentence>
```

- Imperative mood, English: `Add`, `Fix`, `Remove`, `Rename`.
- Capitalize the first word; no trailing period.
- No `type:` or `type(scope):` prefix, no `[tag]`.
- 72 characters or fewer.

```text
✨ Add HTTP routing foundation
🐛 Fix redirect status for mounted subtrees
```

| Emoji | Use for | Label |
| --- | --- | --- |
| ✨ | Introduce new features | `feature` |
| 🐛 | Fix a bug | `bug` |
| 💥 | Introduce breaking changes | `breaking` |
| 🔒️ | Fix security or privacy issues | `security` |
| 📝 | Add or update documentation | `docs` |
| ✏️ | Fix typos | `docs` |
| 🌐 | Internationalization and localization | `lang-all` |
| ⬆️ | Upgrade dependencies | `upgrade` |
| ♻️ | Refactor code | `refactor` |
| 🎨 | Improve structure and format of the code | `refactor` |
| ⚡️ | Improve performance | `refactor` |
| 🔥 | Remove code or files | `refactor` |
| 👷 | Add or update CI build system | `internal` |
| 🔧 | Add or update configuration files | `internal` |
| 🔨 | Add or update development scripts | `internal` |
| 🚨 | Fix compiler and linter warnings | `internal` |
| ✅ | Add, update, or pass tests | `internal` |
| 🔖 | Release and version tags | `release` |

Any other emoji is accepted but stays unlabelled; see
[gitmoji.dev](https://gitmoji.dev) for the full list.

Pull request titles use the same format, because a squash merge turns the
title into the commit title on `main`. Finalize the title before review, and
leave labels to the emoji.

## Go conventions

- Follow [Effective Go](https://go.dev/doc/effective_go) and the
  [code review comments](https://go.dev/wiki/CodeReviewComments).
- Format with `gofmt`.
- Prefer small, explicit APIs and composition.
- Keep dependencies minimal and independent subsystems uncoupled.
- Add table-driven tests for bug fixes.
- Preserve standard library behavior unless the change is deliberate and
  documented.

From the module root:

```sh
gofmt -l .
go mod verify
go build ./...
go vet ./...
go test -race ./...
```

## Pull requests

- Explain the motivation, the approach, and how you verified the change.
- Keep unrelated refactoring and large dependencies out of bug fixes.
- Say why a behavior is untested when it is.

No DCO sign-off is required.

## Releases

Release automation uses the pinned
[Release Actions](https://github.com/hainuoli/release-actions) action.

1. Wait for pending Latest Changes runs to finish.
2. In Actions, run **Prepare Release** from `main`. Enter an unused tag such as
   `v0.1.0` and the intended publication date in `YYYY-MM-DD` format. An empty
   date uses today in UTC.
3. Review the generated `🔖 Prepare v0.1.0 release` PR. It moves the accumulated
   CHANGELOG entries under the version heading and preserves Latest Changes.
   The `release` label excludes the preparation PR from changelog entries.
4. Wait for PR checks, merge it, and wait for the latest `main` CI to pass.
5. Pull `main`, verify the date and release notes, and push an annotated tag:

   ```sh
   git switch main
   git pull --ff-only origin main
   git tag -a v0.1.0 -m "🔖 Release v0.1.0"
   git push origin v0.1.0
   ```

The Release workflow validates and tests the tagged code on stable Go and the
minimum supported Go version, then creates the GitHub Release from that tagged
CHANGELOG section. Do not create a second Release manually or move a published
tag; use a new version for corrections.

Preparation uses `RELEASE_ACTIONS_TOKEN` when configured, otherwise the workflow's
`GITHUB_TOKEN`. For automatic PR checks, provide a repository-scoped PAT or GitHub
App token with Contents, Pull requests, and Issues write permissions. The default
workflow token requires **Allow GitHub Actions to create and approve pull
requests** in Actions settings; a PR created with it needs to be closed and
reopened by a maintainer to trigger ordinary PR checks. Reopening does not modify
the prepared changelog. The existing `LATEST_CHANGES_TOKEN` remains dedicated to
default-branch release-note updates.

To recover a partially completed preparation, rerun it with the same version and
date. The action reuses an existing release PR without overwriting its edits.
