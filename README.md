# gh-runners-scan

**TL;DR:** see which GitHub Actions jobs are running or queued across all repos of any GitHub account, and why a job is stuck on *"Waiting for a runner"*. Cancel a run, or re-run a finished one.

| | |
|---|---|
| `gh-runner` | live terminal UI |
| `gh-runners` | same UI in a terminal; tab-separated table when piped |
| [web page](https://bbestbest.github.io/gh-runners-scan/) | browser version |

## Quick start

```sh
./install.sh                   # builds and links gh-runner and gh-runners into ~/.local/bin
gh-runner <owner>              # live view of every repo the account owns
gh-runner <owner> --repo web   # one repo
gh-runners <owner> | cat       # one-shot table
```

Token: `GH_TOKEN`, `GITHUB_TOKEN`, or `gh auth token`.

## Options

| flag | env | default |
|---|---|---|
| `<owner>` | | `scan` in `~/.config/gh-runners/config` (e.g. `scan = my-org/*, other-org/api`, saved by `O`), else `org`, else your own account |
| `--days N` (repos pushed within N days, 0 = all) | `GH_RUNNERS_DAYS` | 7 |
| `--label`, `--repo`, `--interval` (TUI), `--select` (fzf) | | |

## Keys

`j`/`k` move · `h`/`l` switch panel (`1` running, `2` queued, `3` finished) · `/` filter · `enter` open · `c` cancel run · `R` re-run a finished run (`f` failed jobs, `a` all jobs) · `r` rescan · `O` choose what to scan: a checklist of accounts (`space` all/none) and their repos (`enter`), `s` saves (applies at the next scan; `r` for now) · `u` API usage · `q` quit

## Reading it

Times are in your local time zone, labelled like `10:04 UTC+7`, in both the TUI and the web page. Running jobs show their current step (`3/4`, and `3/4 · Run tests` in the detail). The terminal or browser tab title shows the queued count, e.g. `(2 queued) gh-runner`. On start or reload both show the last results at once, marked `last results from …` in yellow, until the new scan replaces them (TUI: `~/.cache/gh-runners/last.json`; web: inside the encrypted cache, only with "Remember").

Rescans only look at repos that were pushed since the last round or still have jobs, plus a rolling eighth of the quiet ones, so each quiet repo is re-checked every 8 rounds. Repos with no workflows are checked once a day and otherwise skipped. Rows appear as each repo finishes. On a 75-repo account every round takes about 3–4s instead of an 18s full scan, and a restart within 30 minutes picks up where it left off. A first run checks recently pushed repos at once and the rest over the next 7 rounds; Status shows `first pass · N repos not checked yet` until all are covered.

**Finished** lists the last 5 finished runs of each checked repo, from the past 24 hours, with their result. Cancel and re-run ask first; GitHub cancels or re-runs the whole run, never one job. Both need a token that can write Actions (see below).

Queued and running jobs with the same labels: runners are **busy**. Queued with nothing running: runners may be **offline**.

## Web version

1. Open the [page](https://bbestbest.github.io/gh-runners-scan/) and paste a fine-grained token: resource owner = the account that owns the repos, **Actions: Read-only**, or **Read and write** to cancel and re-run, with an expiry date.
2. Choose what to scan, same as `O` in the TUI: tick accounts (all their repos) or open "repos" to tick single ones; "Add owner" adds any other user or organisation. Several owners show repos as `owner/repo`. Each repo shows when it was last pushed, and "filter repos…" narrows the list. As in the TUI, a changed choice, "Pushed within" or rescan interval applies at the next scan; the current results keep running.
3. Optional: tick "Remember". The token is stored encrypted under a passphrase that is never stored. Let Chrome save the passphrase, and later visits open straight to the dashboard. Within a tab, the unlocked token and view are kept encrypted too (AES-GCM under a random, non-exportable key in IndexedDB), so nothing readable is ever written to browser storage. With "Remember" on, the API cache is kept between visits too, like the TUI's disk cache: AES-GCM-encrypted in IndexedDB under a key derived from the token, holding only the fields the page uses. Reloads are then mostly free `304`s. "Forget token" or a rejected token deletes it.

The page talks only to `api.github.com`. It changes something only when you confirm a cancel or re-run. It's free to host on a public repo.

**Deploy:** bump `web/VERSION` (e.g. `1.0.0` → `1.0.1`) and push to `main`. Pushes without a bump don't deploy.

## Limits

Up to 20 queued and 20 running runs per repo; finished runs: the last 5 per repo within 24 hours, refreshed when the repo is checked; repos older than `--days` are skipped.

## License

[MIT](LICENSE)
