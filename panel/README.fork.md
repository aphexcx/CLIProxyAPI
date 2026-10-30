# Forked management panel

This directory is a Git subtree of
https://github.com/router-for-me/Cli-Proxy-API-Management-Center at
`64b4f5aed19b8f791eb21eb9e785ea379ea05b44`. The import preserves the upstream
history and license. Local changes consume the hub's v8 Claude usage cache.

From the repository root, with Node 24 and pnpm available, run:

```sh
make panel-build
```

This uses the panel's pinned Bun 1.4.3 and frozen `bun.lock`, then produces the
single-file `panel/dist/management.html`. No generated asset is committed.
The default `PANEL_VERSION` is fixed so Git tags and commit count cannot change
the output. Releases may set `PANEL_VERSION`, but comparisons must use the same
value, toolchain and lockfile. An existing Bun 1.4.3 installation can be used
with `make panel-build BUN=/path/to/bun`.

Run `make panel-test` for the panel suite. Run the panel's `bun run verify`
with Bun 1.4.3 for tests, lint and build; the repository's Go checks remain
separate. Upstream package scripts and tests are retained.

Credential-list ingestion seeds Claude cards without per-seat network reads.
Usage reads use GET `/v8/management/credentials/quota`; explicit refresh uses
POST and queues the hub's shared poll loop. A refresh response is the current
cache, not proof of a new successful upstream poll. The server's poll timestamp,
age, next eligible slot, queued state and retained failure state remain visible.
Profile metadata is optional enrichment after an explicit refresh and cannot
delay or replace usage from a newer poll.

When native polling is disabled, the server omits `claude_usage` and returns
404 for the cache endpoint. Only an explicit refresh may then fall back to the
existing manual OAuth usage request. Ordinary reads and failures for a known
native-cache seat never trigger that fallback. Reset-grant reads on the quota
page require the separate **Check reset grants** action; cached quota appearing
on screen does not start an upstream grant request.

Publishing the release asset, changing `management.panel-github-repository`,
and deploying the hub are separate rollout operations. Periodic panel updates
run every three hours unless `management.disable-auto-update-panel` is true;
that flag still permits downloading an absent asset on first access.
