# gh-runners-scan

**TL;DR:** see which jobs hold an org's self-hosted GitHub Actions runners, and why a job is stuck on *"Waiting for a runner"*.

| | |
|---|---|
| `gh-runner` | live terminal UI |
| `gh-runners` | same UI in a terminal; tab-separated table when piped |
| [web page](https://bbestbest.github.io/gh-runners-scan/) | read-only browser version |

## Quick start

```sh
./install.sh                 # builds and links gh-runner and gh-runners into ~/.local/bin
gh-runner <org>              # live view
gh-runner <org> --repo web   # one repo
gh-runners <org> | cat       # one-shot table
```

Token: `GH_TOKEN`, `GITHUB_TOKEN`, or `gh auth token`.

## Options

| flag | env | default |
|---|---|---|
| `<org>` | `GH_RUNNERS_ORG` | from `~/.config/gh-runners/config` |
| `--days N` (repos pushed within N days, 0 = all) | `GH_RUNNERS_DAYS` | 7 |
| `--label`, `--repo`, `--interval` (TUI), `--select` (fzf) | | |

## Keys

`j`/`k` move · `h`/`l` switch panel · `/` filter · `enter` open · `c` cancel run · `r` rescan · `O` org · `u` API usage · `q` quit

## Reading it

Queued and running jobs with the same labels: runners are **busy**. Queued with nothing running: runners may be **offline**.

## Web version

1. Open the [page](https://bbestbest.github.io/gh-runners-scan/) and paste a fine-grained token: resource owner = the org, **Actions: Read-only**, with an expiry date.
2. Pick the org, optionally a repo, and the settings.
3. Optional: tick "Remember". The token is stored encrypted under a passphrase that is never stored. Let Chrome save the passphrase, and later visits open straight to the dashboard.

The page talks only to `api.github.com` and never changes anything. It's free to host on a public repo.

**Deploy:** bump `web/VERSION` (e.g. `1.0.0` → `1.0.1`) and push to `main`. Pushes without a bump don't deploy.

## Limits

Only queued and running jobs; up to 20 runs per status per repo; repos older than `--days` are skipped.
