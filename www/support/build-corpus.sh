#!/usr/bin/env bash
set -euo pipefail
ref=$(git rev-parse --verify "${1:-HEAD}^{commit}")
out=${2:?provide output .tar.gz}
# git archive fails on unmatched pathspecs; use the tracked files from the pinned tree.
git ls-tree -r --name-only "$ref" -- cmd internal desktop ui mobile lib scripts deploy/relay/README.md deploy/cloud/README.md docs/guide README.md agent-vm.sh agent-vm.setup.sh runtime.example.sh |
  awk '/^(README\.md|(mobile|cmd\/cage-mobile-link|deploy\/(relay|cloud))\/README\.md|docs\/guide\/[^\/]+\.md)$/ || /^(agent-vm(\.setup)?|runtime\.example)\.sh$/ || (/^(cmd|internal|desktop|ui|mobile|lib|scripts)\// && /\.(go|swift|cs|ts|tsx|jsx|mjs|js|sh|kt|java|ps1|m|h|c|py)$/ && !/([Tt]est|[Ss]moke|\/vendor\/|\/node_modules\/|\/fixtures\/)/) {print}' > "$out.paths"
trap 'rm -f "$out.paths"' EXIT
files=()
while IFS= read -r file; do files+=("$file"); done < "$out.paths"
git archive --format=tar.gz --prefix=source/ -o "$out" "$ref" -- "${files[@]}"
printf '%s\n' "$ref" > "$out.rev"
