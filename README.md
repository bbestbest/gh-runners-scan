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

The org comes from the first argument, or `GH_RUNNERS_ORG`. With neither, both commands exit 1.

## Configuration

| variable | meaning | default |
|---|---|---|
| `GH_RUNNERS_ORG` | organisation to scan | none — required |
| `GH_RUNNERS_DAYS` | only scan repos pushed within N days (0 = all) | 7 |
| `GH_TOKEN` / `GITHUB_TOKEN` | API token; falls back to `gh auth token` | — |

Flags: `--label`, `--days`, `--repo`, `--once`, `--interval` (TUI), `--select` (CLI).

## Reading the output

**Running** is sorted by runner name, so two jobs on the same runner sit together. **Queued** is sorted oldest first — the top row has waited longest.

Runner *labels* (`self-hosted,ARM64`) are what a job requests. The runner *name* (`github-runner-arm64-0-1`) is the machine that took it. A job queued with labels that match a runner already running something is waiting for capacity, not misconfigured.

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
