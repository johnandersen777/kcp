# Licensing

This repository is a fork of [kcp-dev/kcp](https://github.com/kcp-dev/kcp)
and has two licenses.

## Apache-2.0: upstream code

Everything inherited from github.com/kcp-dev/kcp, including files this fork
modified, remains under the Apache License 2.0. See [LICENSE](LICENSE).

## Unlicense: files this fork adds

Files added by this fork (relative to the upstream merge-base
`1a4cb725de077a081365b2ce40bb3e5e8006efa3`) are released into the public
domain under the Unlicense. See [UNLICENSE](UNLICENSE).

To list them:

```sh
git diff --diff-filter=A --name-only 1a4cb725de077a081365b2ce40bb3e5e8006efa3 HEAD
```

Existing per-file "Copyright The KCP Authors" headers on fork-added files are
historical; for those files the Unlicense applies.

## Exceptions

These directories replace or derive from third-party files and keep their
original licenses, not the Unlicense:

| Directory | License |
| --- | --- |
| `contrib/wasm/_overlay/` | Apache-2.0 (Kubernetes, etcd, moby) |
| `contrib/wasm/kubectl/_overlay/` | Apache-2.0 (kubectl, cli-runtime) |
| `contrib/wasm/goat/indigo/` | Bluesky indigo's license |
