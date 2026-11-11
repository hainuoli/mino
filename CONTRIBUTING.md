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
