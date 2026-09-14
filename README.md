# Git Snapshot

Create named, local snapshots of a Git working tree without changing its
files, staging area, or branch history. Snapshots include non-ignored untracked
files and preserve staged contents separately.

## Build

Requires Windows, a current Git installation on PATH, and Go 1.27.1 to build.
The application uses only the Go standard library.

```console
go build -trimpath -buildvcs=false -o snapshot.exe .
go test -count=1 .
```

Add the executable's directory to PATH, or invoke it by its full path from
the repository you want to snapshot. `-trimpath` and `-buildvcs=false` omit
local source paths and repository revision metadata from the executable.

## Commands

| Command | Description |
| --- | --- |
| `snapshot` or `snapshot save` | Save the current repository state. Names use the branch and local time, such as `main-2026-09-13-T-11-40`; collisions receive a numeric suffix. |
| `snapshot list` | List snapshots for the current worktree, newest first. |
| `snapshot diff` | Compare the latest snapshot for the current branch with current files, including untracked additions and edits. |
| `snapshot diff <name>` | Compare a named snapshot with current files. |
| `snapshot diff <older> <newer>` | Compare two saved snapshots. |
| `snapshot diff --index [<name> [<name>]]` | Compare staged contents instead of working files, using current or saved staging areas. |
| `snapshot save --include-ignored` | Also capture ignored files. Use deliberately: this can include credentials, caches, and build outputs. |
| `snapshot delete <name>` | Remove a snapshot reference without changing working files. Stored objects are not immediately erased. |
| `snapshot --help` | Show usage and supported options. |

Diff options include `--stat`, `--name-only`, `--name-status`, `--binary`, and
`--exit-code`. Paths after `--` are literal and relative to the current
directory. Exit codes are `0` for success, `1` for differences with
`--exit-code`, and `2` for errors. Restore, sparse working-file checkouts,
unresolved merges, and submodule/nested-repository contents are not supported.

## Technical TL;DR

The tool uses a temporary Git index to collect selected working files without
altering the real index. A snapshot ref under `refs/snapshots/v1/` retains a
working-file commit, its staged-content parent, and the original `HEAD`;
unchanged objects are shared by Git. Comparisons use trees on both sides,
including a temporary current tree when needed, so untracked changes are
visible. This is a Git-content checkpoint, not a filesystem backup: Git
filters and line-ending rules apply, and timestamps, ACLs, and empty
directories are not saved. Snapshot refs are local by default, but explicit
refspecs or mirror pushes can publish them.
