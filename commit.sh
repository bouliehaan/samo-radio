#!/usr/bin/env bash
#
# commit.sh — gate the working tree, commit it, and get it onto GitHub.
#
#   ./commit.sh "what changed and why"
#   ./commit.sh --skip-gates "docs only"
#   ./commit.sh                          # no message: just push what is already committed
#
# The job is not finished when the commit object exists. It is finished when
# origin agrees, so this pushes and then re-checks the remote rather than
# trusting its own exit code.

set -euo pipefail

BRANCH="main"
SKIP_GATES=0

for arg in "$@"; do
  case "$arg" in
    --skip-gates|-n) SKIP_GATES=1; shift ;;
  esac
done

MESSAGE="${*:-}"

# ---------------------------------------------------------------- preflight

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "Error: not inside a git repository." >&2
  exit 1
fi

# Run from the repo root whatever directory this was invoked from, so `git add`
# below stages the whole repo and not just the subtree you happen to be in.
cd "$(git rev-parse --show-toplevel)"

CURRENT="$(git rev-parse --abbrev-ref HEAD)"
if [ "$CURRENT" != "$BRANCH" ]; then
  # Refusing is the point. `git add . && git commit` is perfectly happy to bury
  # an afternoon's work on a detached HEAD or a stray branch, and you would not
  # find out until the next time you looked for it.
  echo "Error: on '$CURRENT', expected '$BRANCH'." >&2
  echo "       git switch $BRANCH" >&2
  exit 1
fi

# --porcelain, NOT `git diff --quiet`: diff only sees files git already tracks,
# so a change made entirely of NEW files reads as "nothing to do".
DIRTY="$(git status --porcelain)"

git fetch --quiet origin "$BRANCH"
AHEAD="$(git rev-list --count "origin/$BRANCH..HEAD")"

if [ -z "$DIRTY" ] && [ "$AHEAD" -eq 0 ]; then
  echo "Nothing to do — tree is clean and origin/$BRANCH is up to date."
  exit 0
fi

if [ -n "$DIRTY" ] && [ -z "$MESSAGE" ]; then
  echo "Error: uncommitted changes need a message." >&2
  echo "       ./commit.sh \"what changed and why\"" >&2
  exit 1
fi

# -------------------------------------------------------------------- gates

if [ "$SKIP_GATES" -eq 0 ]; then
  echo "==> Formatting..."
  # -print0/-0 because a path with a space would otherwise split into two
  # arguments and gofmt would fail on both halves.
  find . -name '*.go' -not -path './.git/*' -not -path '*/node_modules/*' -print0 \
    | xargs -0 gofmt -l -w

  echo "==> go vet"
  go vet ./...

  echo "==> go test"
  go test ./...
else
  echo "==> Gates SKIPPED (--skip-gates)"
fi

# ------------------------------------------------------------------- commit

if [ -n "$DIRTY" ]; then
  echo "==> Changes:"
  git status --short

  # Re-read: gofmt may have touched files that were clean a moment ago.
  git add -A
  git commit -m "$MESSAGE"
fi

# --------------------------------------------------------------------- push

# Fetch again — the gates may have taken minutes, and a push rejected for being
# behind is the single most common way this script used to end in a mess.
git fetch --quiet origin "$BRANCH"

if [ "$(git rev-list --count "HEAD..origin/$BRANCH")" -gt 0 ]; then
  echo "==> origin/$BRANCH moved; rebasing onto it..."
  # Stops here on conflict or on unrelated histories, which is correct: both
  # need a human, and neither should be resolved by a script holding a commit.
  git pull --rebase origin "$BRANCH"

  if [ "$SKIP_GATES" -eq 0 ]; then
    echo "==> Re-running tests after rebase..."
    go test ./...
  fi
fi

echo "==> Pushing to origin/$BRANCH..."
git push origin "$BRANCH"

# ------------------------------------------------------------------- verify

git fetch --quiet origin "$BRANCH"
read -r BEHIND STILL_AHEAD <<<"$(git rev-list --left-right --count "origin/$BRANCH...HEAD")"

if [ "$BEHIND" -eq 0 ] && [ "$STILL_AHEAD" -eq 0 ] && [ -z "$(git status --porcelain)" ]; then
  echo "==> Done. origin/$BRANCH == $(git rev-parse --short HEAD), tree clean."
else
  echo "Error: still out of sync (behind $BEHIND, ahead $STILL_AHEAD)." >&2
  exit 1
fi
