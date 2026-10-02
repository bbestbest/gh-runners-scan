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
gh-runners <org> --repo web       # scan one repo, not the whole org
```

The org comes from the first argument, `GH_RUNNERS_ORG`, or the config file. With none of them, both commands exit 1.

## Configuration

| variable | meaning | default |
|---|---|---|
| `GH_RUNNERS_ORG` | organisation to scan | none — required |
| `GH_RUNNERS_DAYS` | only scan repos pushed within N days (0 = all) | 7 |
| `GH_TOKEN` / `GITHUB_TOKEN` | API token; falls back to `gh auth token` | — |

Flags: `--label`, `--days`, `--repo`, `--once`, `--interval` (TUI), `--select` (CLI).

`--repo` scans only that repo instead of the whole org, and accepts either a bare name resolved against the org or an explicit `owner/repo`. In the TUI it also highlights the repo's rows.

### Config file

`org`, `orgs` and `days` can be stored in `$XDG_CONFIG_HOME/gh-runners/config`, or `~/.config/gh-runners/config` when `XDG_CONFIG_HOME` is unset. One `key = value` per line; `#` starts a comment.

```ini
# scan this org unless a different one is given
org = <org>
orgs = <org>, <other-org>
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
│  > <org>  (saved)      │
│    <other-org>         │
│                        │
│  ↑↓ · x · enter · esc  │
╰────────────────────────╯
```

`↑`/`↓` walk the list and copy the highlighted org into the field; `↑` past the top returns to what you had typed. Typing anything clears the highlight, so the typed value is what `enter` accepts. `j` and `k` stay literal characters — the field is a text input. With no history the box is just the input and the hint, as before.

`x` deletes the highlighted org from the list, so a typo or an org you no longer scan stops being offered. It only deletes while a row is highlighted; with no highlight `x` is an ordinary character typed into the field, like `j` and `k`. Deleting is a list operation and nothing more: only the `orgs` line is rewritten, `org` keeps its value, and the session carries on scanning the same org without a rescan. Deleting the org that `org` names therefore drops it from the list for that session, but the next run reads `org` and offers it again — the saved org is always part of the history. A delete that cannot be written shows `org not saved` and still applies in-session.

The field itself always opens **empty**, showing the `organisation` placeholder, whether the overlay comes up at startup or from `<O>`. The saved org is still listed below and still marked `(saved)` — the field starts blank so typing a new org needs no clearing first, and `↑`/`↓` are there when you want the one you already have. `enter` on an empty field is a cancel: it keeps the current org for the session and saves nothing, the same as `esc`.

`gh-runners` (the CLI) never writes the file.

## Keys

| key | action |
|---|---|
| `↑`/`↓`, `j`/`k` | move one row |
| `gg` / `home` | jump to the first row |
| `G` / `end` | jump to the last row |
| `pgup` / `pgdown`, `ctrl+b` / `ctrl+f` | move one page |
| `ctrl+u` / `ctrl+d` | move half a page |
| `H` / `M` / `L` | jump to the top, middle or bottom row of the visible window |
| `tab`, `1`, `2` | switch between Running and Queued |
| `/`, `esc` | filter, clear the filter |
| `enter` / `o` | open the job in the browser |
| `r` | rescan now |
| `O` | switch org |
| `c` | cancel the selected job's workflow run |
| `u` | API usage overlay; `esc`, `u` or `q` closes it |
| `q` | quit |

The vim keys work in normal mode only. The first `g` arms a pending state; a second `g` jumps to the top, and any other key (including a lone `g` followed by `j`) clears it. While the filter, the org overlay or the cancel prompt is open they are ordinary typed characters, or abort the prompt. There is no count prefix (`5j`) because `1` and `2` already switch panels, and no `n`/`N` search; `n` only answers the cancel prompt.

`c` asks first, in the status line:

```
cancel run <workflow> (<repo>)? stops every job in this run · y/n
```

`y` sends the cancel; any other key aborts. GitHub has no per-job cancel, so this stops **every job in the run**, not only the selected one. The row shows `cancelling` until a later scheduled scan no longer lists it, and the status line reports the outcome: accepted, already finished, no permission, or rate limited. A run whose cancel is in flight or already accepted cannot be cancelled again in the same session. A cancel does not trigger a rescan. `c`, `y` and `n` are ordinary typed characters while the filter or the org overlay is open.

Cancelling needs a token that can write Actions: `actions:write` on a fine-grained token, or the `repo` scope on a classic one. Without it GitHub answers 403/404 and the status line says so. Rerunning a finished run is not supported; completed runs are out of scope.

## Reading the output

**Running** is sorted by runner name, so two jobs on the same runner sit together. **Queued** is sorted oldest first — the top row has waited longest.

Runner *labels* (`self-hosted,ARM64`) are what a job requests. The runner *name* (`runner-1`) is the machine that took it. A job queued with labels that match a runner already running something is waiting for capacity, not misconfigured.

## API usage

Each scan costs roughly `1 + 2×repos` requests, plus one more per active run (and one per extra 100 jobs in a run), so the org size drives the bill against the 5000/hour quota. `--repo` skips the repo listing entirely and costs about 2–3 requests.

Conditional requests keep that cheap: an unchanged repo returns `304 Not Modified`, which does not count against the quota. The ETag cache is persisted between runs (see [ETag cache on disk](#etag-cache-on-disk)), so:

- the first scan after a restart or a new CLI invocation reuses the last run's ETags and mostly gets free `304`s
- a cold scan (no cache file, other token, or entries GitHub has since changed) pays full price

A cancel (`c`) is one `POST /repos/{owner}/{repo}/actions/runs/{run_id}/cancel`. It is never cached, counts toward the 5000/hour quota like any other call, and also counts toward GitHub's secondary content-creation limit, so mashing it on many runs can trip that limit. It does not start a rescan.

The timer never starts a scan while one is still running: the countdown pauses during a scan and restarts from `--interval` when it ends, so a scan that outlasts the interval makes the real cycle scan time plus the interval. `r` is ignored while a scan runs.

Scans run 6 workers. Raising that trips GitHub's *secondary* rate limit (burst detection), which is separate from the hourly quota and invisible to `gh api rate_limit`.

When a scan is rate-limited it says so on stderr and exits 2, rather than printing an empty table that looks like an idle org.

### ETag cache on disk

`gh-runners` and `gh-runners-tui` share one cache file: `$XDG_CACHE_HOME/gh-runners/etags.json`, or `~/.cache/gh-runners/etags.json` when `XDG_CACHE_HOME` is unset. The directory is created `0700` and the file `0600`. It is written after every finished scan and when the TUI quits, and only holds the URLs the last run actually used.

The file holds each response reduced to the fields the tool reads: repo names, run ids, job, workflow, branch and runner names, labels, times and URLs. It holds no emails, commit messages, logins or descriptions. It is keyed by a SHA-256 of the token, never the token itself, so switching `GH_TOKEN` starts cold. A missing, corrupt or old-version file is ignored.

To clear it: `rm ~/.cache/gh-runners/etags.json` (or under `$XDG_CACHE_HOME`).

### Rate limit in the TUI

The right side of the status line always shows the quota, for example `api 4084/5000 · resets 16:42` (reset time in local time). It turns yellow below 20% remaining and red below 5%. Before the first response arrives it shows `api –`.

`u` opens an overlay with the resource, limit, used, remaining, the reset time with a countdown, requests sent this session, how many came back `304` (free), how many were rate limited, the cost of the last scan (`N requests, M free (304)`) and the current `--interval`. `esc`, `u` or `q` closes it; other keys, including the table keys, do nothing while it is open. With no response yet it says `no data yet`.

The numbers are read from the `X-RateLimit-*` headers of the responses the tool itself receives, with no extra request. They are the token's real counters, so they include usage by anything else sharing the same token. The request, `304` and rate-limited counts cover only this process.

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
- At most 20 in-progress and 20 queued runs per repo per status. A run's jobs are fetched 100 per page and every page is followed.
- Completed jobs are out of scope by design.
