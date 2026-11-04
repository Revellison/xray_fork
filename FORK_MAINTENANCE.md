# Fork maintenance guide

For future agents and maintainers. Read this before syncing upstream, resolving
conflicts, changing CI or publishing a release. Feature behaviour and config are in
[docs/FORK_NOTES.md](docs/FORK_NOTES.md).

## What this repository is

[XTLS/Xray-core](https://github.com/XTLS/Xray-core) plus two additions:

- **Part A, speed limiter:** `app/speedlimit`, `infra/conf/speedlimit.go`, wired in at
  `app/dispatcher/default.go` (`speedlimit.Default().User(...)`, `WrapUp`/`WrapDown`).
- **Part B, extended sniffing:** QUIC v2, ClientHello reassembly (QUIC and TLS),
  BitTorrent uTP/DHT/UDP tracker, ported from Jolymmiles/Xray-core.

## Fork hooks in upstream files

Fork logic lives in new files. Upstream files carry only these hooks. If a merge loses
one, the feature silently stops working, so check them after every sync:

```bash
grep -rn "// fork" app/dispatcher/default.go transport/internet/splithttp/ infra/conf/xray.go
grep -n "applySpeedlimitUploadDefaults\|SetEmbeddedConfig" infra/conf/xray.go
```

| Upstream file | Hook | Why |
|---------------|------|-----|
| `app/dispatcher/default.go`, `getLink` | `limitPipeLink(...)` | Limits `Dispatch` paths: Trojan, VMess, Shadowsocks, mux |
| `app/dispatcher/default.go`, `WrapLink` | `limitDirectLink(...)`, **before** `TimeoutWrapperReader` | Limits `DispatchLink` paths: VLESS on any transport, SOCKS, HTTP, Hysteria, TUN |
| `transport/internet/splithttp/hub.go`, POST branch | `enterPostRead` / `leavePostRead`, **before** the body is read | Stops packet-up uploads stalling a shared connection |
| `transport/internet/splithttp/upload_queue.go` | `postSlots` field and its `make` | Used by the hook above |
| `infra/conf/xray.go` | `Speedlimit` field, `Override` branch, `Build` block | Reads the config and sets upload buffer defaults |

The heavy exception is the sniffer: `common/protocol/quic/sniff.go`, `tls/sniff.go` and
`bittorrent/bittorrent.go` are rewritten, not hooked. See the conflict table below.

## Rules

1. **Merge upstream; never rebase or force-push `main`.** Release tags point into its
   history. This replaces older advice to rebase.
2. **Green merge is not enough.** A clean merge can fail to compile. Always build and test.
3. **Never drop a fork feature to get a merge through,** and never skip or delete a
   failing test to go green. Find out why it fails.
4. **Do not push to upstream.** Locally its push URL should be `DISABLED`.
5. **Push explicit refs only:** `git push origin main`. Never `--all`, `--mirror` or
   `--tags`. Do not fetch upstream's tags into this repository: release numbering reads
   the local tag list, and `sync-upstream.sh` does not need them.
6. **Keep fork changes out of upstream files where possible.** Prefer new files
   (`fork-*.yml`, `app/speedlimit/`). Every line in an upstream file is a future conflict.
7. **Keep the module path `github.com/xtls/xray-core`.** Renaming it touches every file
   and makes every sync conflict.

## Frozen branch

`stable/v26.10.31-fork` is the known-good fallback and **never takes upstream merges**.
`sync-upstream.sh` and `fork-sync-upstream.yml` refuse `stable/*`. To go back to it, deploy
its release, or branch from it to fix something. Create a new `stable/<version>-fork`
branch after a verified release instead of moving an old one.

## History

`main` descends from upstream's real history, which is what makes merges possible. The fork
was imported on top of tag `v26.10.31` as two commits, so its original per-feature commits
no longer exist. To see everything the fork changes:

```bash
git fetch upstream main
git diff --stat "$(git merge-base upstream/main refs/heads/main)" refs/heads/main
```

Never replace `.git` or re-create the repository from a file copy. That cuts the link to
upstream, and every sync then fails with `refusing to merge unrelated histories`. Do not
"fix" that error with `--allow-unrelated-histories` on an upstream merge. Restore the
shared history instead.

## Local setup

```bash
git remote -v
# origin    <this repository>
# upstream  https://github.com/XTLS/Xray-core.git  (push: DISABLED)

# If upstream is missing:
git remote add -t main --no-tags upstream https://github.com/XTLS/Xray-core.git
git remote set-url --push upstream DISABLED

# Reuse conflict resolutions between attempts:
git config rerere.enabled true
```

## Syncing upstream

### From GitHub (normal path)

Actions → **Fork Sync Upstream** → Run workflow, or:

```bash
gh workflow run fork-sync-upstream.yml                         # latest upstream release, then release
gh workflow run fork-sync-upstream.yml -f upstream_ref=v26.11.1
gh workflow run fork-sync-upstream.yml -f dry_run=true         # check only, push nothing
```

It merges, builds, vets, tests, pushes `main`, cross-compiles every target and publishes
`v<version>-fork.<n>`. On a conflict or failed verification nothing is pushed and an issue
titled `Upstream sync failed: <ref>` is opened. Then resolve locally.

Prefer `latest-release` over `main`: upstream `main` can move past tested code, and
`core/core.go` then reports the previous version number.

### Locally (when CI reports a problem)

```bash
git switch main && git pull --ff-only origin main
bash .github/fork/sync-upstream.sh v26.11.1   # or latest-release, main, <sha>
```

| Exit | Meaning | Next step |
|------|---------|-----------|
| 0 | Merged and verified, not pushed | Review the "changed by both" list, then push |
| 10 | Already up to date | Nothing |
| 20 | Conflict, merge left in progress | [Resolving conflicts](#resolving-conflicts) |
| 30 | Clean merge, build or tests failed | [Semantic conflicts](#semantic-conflicts) |

After pushing, publish with `gh workflow run fork-release.yml` and close the sync issue.

## Resolving conflicts

Understand both sides first:

```bash
git diff --name-only --diff-filter=U           # what conflicted
git log --merge --oneline -- <file>            # commits involved from both sides
git show <upstream-commit> -- <file>           # why upstream changed it
```

Never use `git checkout --ours/--theirs` or `-X ours/theirs` on a whole file listed below:
each silently deletes one side.

| File | Who owns what | How to resolve |
|------|---------------|----------------|
| `infra/conf/xray.go` | Small fork hooks: import, `Speedlimit` field, `Override` branch, `Build` block | Keep both. Keep upstream's order; the inbound module must stay last in `Build` |
| `app/dispatcher/default.go` | Limiter wrap in `getLink`; `cachedReader.Datagrams()`; sniffing loop | Keep both. The limiter must stay the outermost wrapper, around the stats writer, so statistics count traffic that has passed the limiter |
| `app/dispatcher/sniffer.go`, `fakednssniffer.go` | Fork's sniffer registry and `SniffDatagrams` | Keep the fork's structure; port upstream's logic into it |
| `common/protocol/quic/sniff.go` | Heavily rewritten by the fork | Keep the fork's version, then **re-apply upstream's intent by hand** |
| `common/protocol/tls/sniff.go` | Fork's multi-record reassembly; different return signatures | Same. Keep the fork's structure, add upstream's fix |
| `common/protocol/bittorrent/bittorrent.go` | Rewritten by the fork | Same |
| `common/protocol/bittorrent/bittorrent_test.go` | Deleted by the fork; tests moved | If upstream changed it, port its new cases to the fork's tests. Keep it deleted |
| `*.pb.go` | Generated | Take upstream's, then regenerate if needed (`go run ./infra/vprotogen/main.go`) |
| `go.mod`, `go.sum` | Not changed by the fork | Take upstream's, then `go mod tidy` |
| `core/core.go` version | Upstream | Always upstream's. The fork number belongs in the tag only |
| `.github/workflows/*` not named `fork-*` | Upstream | Take upstream's. See [CI](#ci) if the release matrix changed |
| `README.md` | Upstream, plus the fork notice at the top | Keep the notice, take the rest from upstream |

Finish:

```bash
git diff --check          # no markers left behind
git add <files> && git commit --no-edit
go build ./... && go test -count=1 ./app/speedlimit/... ./app/dispatcher/... ./common/protocol/... ./infra/conf/... ./core/... ./testing/fork/...
```

Add one line per non-trivial resolution to the merge commit message.

## Semantic conflicts

Git merges text, not meaning. **Real example from v26.10.31:** upstream added this to
`ReadClientHello` in `common/protocol/tls/sniff.go`:

```go
if nameLen == 0 {
	return errNotClientHello
}
```

The fork had changed that function to return `(string, error)`. Git reported no conflict,
and the compiler failed with `not enough return values`. The fix is `return "", errNotClientHello`.

A newer `go vet` may also flag upstream's dead code. `-unreachable=false` is deliberate:
do not delete upstream's code to satisfy it.

Also check what the compiler cannot:

- **Fixes that bypass the fork's code path.** Upstream's sniffer fixes target upstream's
  code. `9b8190f5` ("QUIC sniffer: Don't accept ClientHello with gaps") changed only
  `tls/sniff.go` while the fork's QUIC reassembly has its own gap logic. Read each upstream
  sniffer commit and confirm that the behaviour holds in the fork, adding a test if not.
- **Changed paths around the limiter:** `buf.Writer`, `getLink`, `WrapLink`, `SizeStatWriter`,
  splice (`CanSpliceCopy`), or how the user's email reaches the dispatcher. A new way to
  move data can silently bypass the limiter, and unit tests will still pass. Only
  `testing/fork` measures real traffic. Never skip or loosen it to get a sync through.
- **Config handling:** `speedLimit` must still parse, and bad values must still refuse to
  start. Test with `xray run -test -c config.json`.

The sync script prints files changed by both sides. Read those diffs even when all is green.

## Limiter design

- **Unlimited users stay on upstream's path.** `MayLimit()` is false, so they get no
  wrappers and keep splice. Limited users lose splice: it would skip the limiter.
- **Shared connections.** Throttling a stream fills its receive buffer. If the buffers
  exceed the connection window, other flows on that connection stall. So when `speedLimit`
  is set, `applySpeedlimitUploadDefaults` gives XHTTP and Hysteria inbounds per-stream
  buffers that are small compared with the connection window. Values set in the config win.
- **Known limitation: mux upload.** Mux has no per-stream flow control, so a throttled upload
  stalls other TCP flows in the same mux tunnel. Download is fine. Fixing it would need a
  mux redesign, so advise leaving mux off for limited users instead.
- **Upload is less smooth than download through a real bottleneck.** The server slows a
  client only through backpressure, after data has crossed the link.

Test on Linux. Windows has no splice, so a pass there proves little.

## CI

| Workflow | Status here | Purpose |
|----------|-------------|---------|
| `fork-sync-upstream.yml` | Fork, manual | Merge, verify, push, build, release |
| `fork-release.yml` | Fork | Tests and builds on push/PR; publishes when run by hand or from sync |
| `test.yml` | Upstream, enabled | Full test suite and format/proto checks |
| `scheduled-assets-update.yml` | Upstream, enabled | Geodata cache used by `test.yml` |
| `release.yml`, `release-win7.yml`, `docker.yml` | Upstream | Superseded by `fork-release.yml`; the owner decides whether they stay enabled. Check with `gh workflow list --all` |

Keep these files in the tree, so merges do not conflict. Turning them off is repository
state, not a file change, and is the owner's decision: they run on every push, and
`release.yml` logs a harmless 403 from `check-assets` when the geodata cache is empty.
`fork-release.yml` does not depend on them. It does not build Windows 7 or Docker images.

`dependabot.yml` comes from upstream and opens PRs on this repository. Do not merge them:
the same bumps arrive with the next sync.

`fork-release.yml` runs `-race` on the packages the fork changes, but not on `./core/...`:
upstream's `testing/servers/udp` helper races on `Server.accepting`, which fails
`TestXrayDialUDP` and `TestXrayDialUDPConn`. Do not patch that helper here; it is upstream's
file. Remove the exception if upstream fixes it.

**Do not edit upstream workflows; edit `fork-*.yml`.** When the sync script warns that the
release matrix changed, port the change to the matrix in `fork-release.yml`.

### Token limits

- A `GITHUB_TOKEN` **cannot push commits that change `.github/workflows/`.** If upstream
  changes those files, sync fails at the push and opens an issue. Either push the verified
  merge locally, or add a `FORK_SYNC_TOKEN` secret (fine-grained PAT for this repository
  only: Contents and Workflows, read/write).
- Pushes and releases made with `GITHUB_TOKEN` do not trigger other workflows. This is why
  the sync workflow calls `fork-release.yml` directly, and why upstream-style
  `on: release` would never fire here.

## Releases

- Tags are `v<upstream version>-fork.<n>`, for example `v26.10.31-fork.1`. `n` resets for
  each upstream version.
- To release fork-only changes: `gh workflow run fork-release.yml`, which bumps `n`.
- A release exists only if tests and all targets succeed, so a failed run leaves no partial
  release. Rerun it after fixing the cause.
- A published binary reports its commit: `Xray 26.10.31 (...) fork-abc1234 (...)`.

## Windows pitfalls

- **`main` is ambiguous** because the repository has a `main/` directory:
  `git log main` fails with `both revision and filename`. Use `git log main --` or
  `refs/heads/main`.
- **`core.autocrlf=true` makes `gofmt -l` list nearly every file.** The working copy has
  CRLF while the blobs have LF. Trust CI (`vformat`) or run `git ls-files --eol`.
- `.github/build/windows/*` are intentionally CRLF. Do not normalize them.
- `go.mod` requires a newer Go than may be installed. Leave `GOTOOLCHAIN=auto` so that Go
  downloads the right toolchain. `-race` needs cgo, so run it on Linux CI.
- `~/.ssh/known_hosts` may not include GitHub. Use HTTPS with `gh auth setup-git`.

## Checklist before pushing a sync

- [ ] No conflict markers: `git diff --check HEAD^1 HEAD`
- [ ] `go build ./...` and the fork package tests pass
- [ ] Diffs in the "changed by both" list were read
- [ ] All hooks in [Fork hooks](#fork-hooks-in-upstream-files) are present
- [ ] `go test ./testing/fork/...` passes on Linux, unmodified
- [ ] `app/dispatcher/default.go` still calls `SniffDatagrams`
- [ ] `xray run -test -c` accepts a config with `speedLimit`
- [ ] `core/core.go` carries upstream's version
- [ ] Non-trivial resolutions are recorded in the merge commit message
- [ ] New pitfalls are added to this file
