# Releasing

`latest` is the only rollout control: every node with auto-update on follows
`releases/latest`. So a release goes out as a prerelease first, gets tested, and
is promoted only after that. Seeds do not follow `latest`; they are updated by
hand.

CI builds the binaries and never holds a signing key. An admin signs on their
own machine, and nothing is public until an admin publishes it.

Signatures are required from v0.5.0. A node on v0.5.0 or later refuses a release
that has no good `SHA256SUMS.sig`, so a release must never go out without one.

## Cut a release

### 1. Tag

Tag a commit on `main` where the `test` workflow is green, using `vX.Y.Z`:

```sh
git tag vX.Y.Z <commit>
git push origin vX.Y.Z
```

`.github/workflows/release.yml` runs the tests, builds the four binaries with
`scripts/release.sh`, checks the asset contract, and creates a **draft
prerelease** holding the binaries and `SHA256SUMS`.

`scripts/release.sh` refuses a real version on a dirty tree, on a `HEAD` that is
not that tag, or when `go run ./scripts/sign check-keys` fails. If the workflow
fails, fix the cause on `main` and tag the next version. If a draft exists for
the tag, delete it before running the workflow again, since the workflow will
not replace it.

### 2. Sign

On the machine that holds your key, in a checkout of the tag:

```sh
gh release download vX.Y.Z --pattern SHA256SUMS
go run ./scripts/sign sign --key /path/to/key --tag vX.Y.Z --sums SHA256SUMS > SHA256SUMS.sig
go run ./scripts/sign verify --tag vX.Y.Z --sums SHA256SUMS --sig SHA256SUMS.sig
gh release upload vX.Y.Z SHA256SUMS.sig
```

`verify` makes the same check a node makes, against the keys in that checkout.
If it fails, do not upload, and do not go on to the next step.

### 3. Publish as a prerelease

```sh
gh release edit vX.Y.Z --draft=false
```

It stays marked as a prerelease. `releases/latest` skips prereleases, so no node
picks it up on its own, and neither do the installers.

### 4. Check the signature and test

Run the new binary against the published prerelease. Use the asset for your
machine:

```sh
gh release download vX.Y.Z --pattern bitllm-linux-amd64
chmod +x bitllm-linux-amd64
./bitllm-linux-amd64 update --check --tag vX.Y.Z
```

It must print `signature: ok`, and it exits non-zero when the signature is not ok.
Do not promote until this passes.

Then try the release on a test node. Run a real request through it and check
`bitllm --version`. On v0.5.0 or later, this installs it:

```sh
bitllm update --tag vX.Y.Z
```

`bitllm update --tag` is new in v0.5.0, and older binaries do not have it, so for
v0.5.0 itself start the test node from the downloaded binary. It also means the
first signed release's own background update path, an older node finding it and
replacing itself, can only be observed by promoting it. Keep a v0.4.6 test node
running to watch that happen.

### 5. Promote

Every seed must run with `--auto-update=false` first, or it replaces itself as
soon as the release is promoted. For `seed1` and v0.5.0, see
[seed-handoff.md](seed-handoff.md).

```sh
gh release edit vX.Y.Z --prerelease=false --latest
```

Nodes with auto-update on now start moving to it, and `install-test` runs the
public installers against it. Watch a test node that still has auto-update on: it
should replace itself and then show `signature: ok` under `update` in
`curl -s 127.0.0.1:7778/status`. Watch the network map too.

### 6. Update the seeds by hand

After 48 hours on `latest`, update each seed, one at a time. Get the script from
the same tag and name the version:

```sh
curl -fsSL -o bootstrap-vps.sh \
  https://raw.githubusercontent.com/bitllm-org/bitllm/vX.Y.Z/deploy/bootstrap-vps.sh
sudo bash bootstrap-vps.sh vX.Y.Z
```

The hostname and the seed it joins are reused from `/etc/bitllm/seed.env`. A seed
installed before that file existed must be given all three arguments once; see
[seed-handoff.md](seed-handoff.md). Check that one seed is healthy before moving
on to the next.

## Back out

Mark the previous release as latest:

```sh
gh release edit vPREVIOUS --latest
```

This stops new updates. It does not bring back nodes that already moved, because
a node only ever moves to a higher version. To reach them, fix the problem and
release a higher tag.

Do not delete a published release or move a tag. Nodes read assets by tag.

## Signing keys

A release is signed if one signature verifies under any key in the list. The list holds the owner's key.

```sh
go run ./scripts/sign keygen /path/to/key
```

This writes the private key to that file, readable only by you, and prints the
public key.

- Keep the private key offline. It never goes in CI, in a repository, or in a
  shared password store.
- Add the public key to `TrustedKeys` in `internal/update/keys.go` through a PR.
  `go run ./scripts/sign check-keys` must pass before a tag can build.
- A new key only counts on nodes that run a release containing it, and those nodes
  refuse a release signed by any other key. Add it at least one release before you
  sign with it.

The list has one key. A key added in a release takes effect for the releases after it, so the release that adds one is still signed with a key already in the list. If the only key is lost, nodes cannot take a signed update, and each one has to be reinstalled by hand.
