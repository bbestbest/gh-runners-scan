# gh-runners-scan

**TL;DR:** see which GitHub Actions jobs are running or queued across all repos of any GitHub account, and why a job is stuck on *"Waiting for a runner"*.

| | |
|---|---|
| `gh-runner` | live terminal UI |
| `gh-runners` | same UI in a terminal; tab-separated table when piped |
| [web page](https://bbestbest.github.io/gh-runners-scan/) | read-only browser version |

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

`j`/`k` move · `h`/`l` switch panel · `/` filter · `enter` open · `c` cancel run · `r` rescan · `O` choose what to scan: a checklist of accounts (`space` all/none) and their repos (`enter`), `s` saves (applies at the next scan; `r` for now) · `u` API usage · `q` quit

## Reading it

Times are in your local time zone, labelled like `10:04 UTC+7`, in both the TUI and the web page. Running jobs show their current step (`3/4`, and `3/4 · Run tests` in the detail). The terminal or browser tab title shows the queued count, e.g. `(2 queued) gh-runner`.

Queued and running jobs with the same labels: runners are **busy**. Queued with nothing running: runners may be **offline**.

## Web version

1. Open the [page](https://bbestbest.github.io/gh-runners-scan/) and paste a fine-grained token: resource owner = the account that owns the repos, **Actions: Read-only**, with an expiry date.
2. Choose what to scan, same as `O` in the TUI: tick accounts (all their repos) or open "repos" to tick single ones; "Add owner" adds any other user or organisation. Several owners show repos as `owner/repo`. Each repo shows when it was last pushed, and "filter repos…" narrows the list. As in the TUI, a changed choice, "Pushed within" or rescan interval applies at the next scan; the current results keep running.
3. Optional: tick "Remember". The token is stored encrypted under a passphrase that is never stored. Let Chrome save the passphrase, and later visits open straight to the dashboard. Within a tab, the unlocked token and view are kept encrypted too (AES-GCM under a random, non-exportable key in IndexedDB), so nothing readable is ever written to browser storage. With "Remember" on, the API cache is kept between visits too, like the TUI's disk cache: AES-GCM-encrypted in IndexedDB under a key derived from the token, holding only the fields the page uses. Reloads are then mostly free `304`s. "Forget token" or a rejected token deletes it.

The page talks only to `api.github.com` and never changes anything. It's free to host on a public repo.

**Deploy:** bump `web/VERSION` (e.g. `1.0.0` → `1.0.1`) and push to `main`. Pushes without a bump don't deploy.

## Limits

Only queued and running jobs; up to 20 runs per status per repo; repos older than `--days` are skipped.

## License

[MIT](LICENSE)
