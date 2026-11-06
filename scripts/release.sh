#!/usr/bin/env bash
# Build release binaries into dist/. -trimpath keeps local paths (user name,
# toolchain layout) out of the binaries; -s -w drops symbol and debug tables;
# -buildvcs=false keeps the build independent of the git state around the tree,
# since the version string already names the tag or commit.
#   scripts/release.sh v0.1.5
#
# A real version (vX.Y.Z) must come from a clean tree with that tag at HEAD and
# with signing keys in place; see docs/releasing.md. Any other version, such as
# the ci-<sha> that CI passes, only builds.
set -euo pipefail
V="${1:?usage: scripts/release.sh vX.Y.Z}"
cd "$(dirname "$0")/.."

# Build with the Go version in go.mod, the one CI tests, whatever is installed here.
go_version=$(awk '/^go /{print $2}' go.mod)
[ -n "$go_version" ] || { echo "release.sh: no go version in go.mod" >&2; exit 1; }
export GOTOOLCHAIN="go$go_version"

if [[ "$V" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  if [ -n "$(git status --porcelain)" ]; then
    echo "release.sh: refusing to build $V: the tree has uncommitted changes" >&2; exit 1
  fi
  if [ "$(git rev-parse -q --verify "refs/tags/$V^{commit}" || true)" != "$(git rev-parse HEAD)" ]; then
    echo "release.sh: refusing to build $V: HEAD is not the tag $V" >&2; exit 1
  fi
  if ! go run ./scripts/sign check-keys; then
    echo "release.sh: refusing to build $V: go run ./scripts/sign check-keys failed" >&2; exit 1
  fi
fi

rm -rf dist && mkdir dist
for t in darwin/arm64 linux/amd64 linux/arm64 windows/amd64; do
  os=${t%/*} arch=${t#*/} ext=""
  [ "$os" = windows ] && ext=.exe
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "-s -w -X github.com/bitllm-org/bitllm/internal/node.Version=$V" -o "dist/bitllm-$os-$arch$ext" ./cmd/bitllm
done
(cd dist && sha256sum bitllm-* > SHA256SUMS)
if grep -l -a -e "$HOME" -e "$(whoami)@" dist/bitllm-* >/dev/null; then
  echo "local paths leaked into dist/" >&2; exit 1
fi
ls -la dist
