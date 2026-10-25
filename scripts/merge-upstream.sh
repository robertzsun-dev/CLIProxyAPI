#!/usr/bin/env bash
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
test "$(git branch --show-current)" = main || { echo "Switch to the fork main first." >&2; exit 1; }
test -z "$(git status --porcelain)" || { echo "Working tree must be clean." >&2; exit 1; }
case "$(git remote get-url origin)" in
 https://github.com/robertzsun-dev/CLIProxyAPI.git|git@github.com:robertzsun-dev/CLIProxyAPI.git) ;;
 *) echo "origin must be Robert's fork." >&2; exit 1 ;;
esac
case "$(git remote get-url upstream)" in
 https://github.com/router-for-me/CLIProxyAPI.git|git@github.com:router-for-me/CLIProxyAPI.git) ;;
 *) echo "upstream must be router-for-me/CLIProxyAPI." >&2; exit 1 ;;
esac
git fetch origin main
git merge --ff-only origin/main
git fetch upstream main
previous=$(git rev-parse HEAD)
git merge --no-ff upstream/main
git merge-base --is-ancestor "$previous" HEAD
git merge-base --is-ancestor upstream/main HEAD
echo "Merge complete. Run regression tests, push origin main normally, then deploy that commit."
