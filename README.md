# gh-runners-scan

Find out which jobs occupy an organisation's self-hosted GitHub Actions runners, and why a workflow is stuck on *"Waiting for a runner to pick up this job"*.

GitHub shows runner status per repository. When a job queues because a runner is busy with work in **another** repo, nothing in that repo's UI tells you so. This scans every repo in the org and shows what is actually running.

## Install

```sh
go build -o bin/gh-runners     ./cmd/gh-runners
go build -o bin/gh-runners-tui ./cmd/gh-runners-tui
```

## Usage

```sh
gh-runners-tui <org>              # live table, rescans on a timer
gh-runners <org>                  # one-shot, tab-separated
gh-runners <org> --label ARM64    # filter by runner label
gh-runners <org> --select         # pick a job in fzf, open it in the browser
```

The org comes from the first argument, `GH_RUNNERS_ORG`, or the config file. With none of them, both commands exit 1.

## Configuration

| variable | meaning | default |
|---|---|---|
| `GH_RUNNERS_ORG` | organisation to scan | none — required |
| `GH_RUNNERS_DAYS` | only scan repos pushed within N days (0 = all) | 7 |
| `GH_TOKEN` / `GITHUB_TOKEN` | API token; falls back to `gh auth token` | — |

Flags: `--label`, `--days`, `--repo`, `--once`, `--interval` (TUI), `--select` (CLI).

### Config file

`org`, `orgs` and `days` can be stored in `$XDG_CONFIG_HOME/gh-runners/config`, or `~/.config/gh-runners/config` when `XDG_CONFIG_HOME` is unset. One `key = value` per line; `#` starts a comment.

```ini
# scan this org unless a different one is given
org = org-a
orgs = org-a, org-b
days = 30
```

`orgs` is the history the `<O>` overlay offers, most recently used first, comma-separated. Only `org` decides what a run scans; `orgs` is there so you can pick instead of retype.

Both commands resolve each value in this order, first hit wins:

1. CLI argument or flag — the positional org, `--days`
2. environment — `GH_RUNNERS_ORG`, `GH_RUNNERS_DAYS`
3. config file
4. built-in default — org: none (exit 1); days: 7

The file is optional. A missing file, an unreadable one, or a `days` that is not a number is ignored silently.

`gh-runners-tui` writes it. Accepting an org in the `<O>` overlay saves `org = <value>` and moves that org to the front of `orgs`, creating the file and its directory if needed, so the next run starts in that org with no prompt — set it once, like `git remote add`. Every accept saves, including a later switch to a different org. The history is capped at 10, dropping the least recently used. A file that has `org` but no `orgs` line reads as a one-entry history. Only the `org` and `orgs` lines change: `days`, comments, blank lines, unknown keys and their order are preserved, and the write is atomic, so an interrupt cannot truncate the file. A save that fails is not fatal — the org still applies for that session and the status line shows `org not saved`.

The overlay lists the orgs from `orgs` under the input, marking the saved one:

```
╭────────────────────────╮
│  org: ▊                │
│                        │
│  > <org>  (saved) │
│    org-b           │
│                        │
│  ↑↓ pick · enter · esc │
╰────────────────────────╯
```

`↑`/`↓` walk the list and copy the highlighted org into the field; `↑` past the top returns to what you had typed. Typing anything clears the highlight, so the typed value is what `enter` accepts. `j` and `k` stay literal characters — the field is a text input. With no history the box is just the input and the hint, as before.

`gh-runners` (the CLI) never writes the file.

## Reading the output

**Running** is sorted by runner name, so two jobs on the same runner sit together. **Queued** is sorted oldest first — the top row has waited longest.

Runner *labels* (`self-hosted,ARM64`) are what a job requests. The runner *name* (`runner-1`) is the machine that took it. A job queued with labels that match a runner already running something is waiting for capacity, not misconfigured.

## API usage

Each scan costs roughly `1 + 2×repos` requests, so the org size drives the bill against the 5000/hour quota.

Conditional requests keep that cheap: an unchanged repo returns `304 Not Modified`, which does not count against the quota. The ETag cache is **in-memory and per-process**, so:

- a long-running `gh-runners-tui` pays full price on its first scan, then close to nothing
- each `--once` invocation is a fresh process and pays full price every time

Scans run 6 workers. Raising that trips GitHub's *secondary* rate limit (burst detection), which is separate from the hourly quota and invisible to `gh api rate_limit`.

When a scan is rate-limited it says so on stderr and exits 2, rather than printing an empty table that looks like an idle org.

### Checking your rate limit

Read the counters off a real request. The `/rate_limit` endpoint is exempt from the quota and can report stale values:

```sh
curl -s -D- -o /dev/null -H "Authorization: token $(gh auth token)" \
  'https://api.github.com/repos/<org>/<repo>/actions/runs?per_page=1' \
  | grep -i 'x-ratelimit-'
```

`x-ratelimit-remaining: 0` is the hourly quota. A 403 with quota still remaining is the secondary limit — reduce concurrency.

## Limitations

- Only repos pushed within `--days` are scanned; a queued job on a dormant repo is missed. Use `--days 0` for everything.
- At most 20 in-progress and 20 queued runs per repo per status.
- Completed jobs are out of scope by design.
