#!/usr/bin/env bash
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
dest="${GH_RUNNERS_BIN_DIR:-$HOME/.local/bin}"
names=(gh-runners gh-runner)
pkgs=(gh-runners gh-runners-tui)

mkdir -p "$repo/bin" "$dest"
cd "$repo"

for i in "${!names[@]}"; do
  n="${names[$i]}"
  tmp="$(mktemp "$repo/bin/.$n.XXXXXX")"
  if ! go build -o "$tmp" "./cmd/${pkgs[$i]}"; then
    rm -f "$tmp"
    echo "build failed: $n" >&2
    exit 1
  fi
  chmod +x "$tmp"
  mv -f "$tmp" "$repo/bin/$n"
done

for n in "${names[@]}"; do
  link="$dest/$n"
  target="$repo/bin/$n"
  if [ -e "$link" ] && [ ! -L "$link" ]; then
    echo "error: $link is a regular file, not overwriting" >&2
    exit 1
  fi
  if [ "$(readlink "$link" 2>/dev/null || true)" != "$target" ]; then
    ln -sfn "$target" "$link"
  fi
done

old="$dest/gh-runners-tui"
if [ "$(readlink "$old" 2>/dev/null || true)" = "$repo/bin/gh-runners-tui" ]; then
  rm -f "$old"
fi
rm -f "$repo/bin/gh-runners-tui"

version="$(git -C "$repo" describe --always --dirty 2>/dev/null || echo unknown)"
for n in "${names[@]}"; do
  echo "$dest/$n -> $repo/bin/$n ($version)"
done
