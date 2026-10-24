# Future work

## Security: secrets baked into a build-time memory snapshot

A pre-booted snapshot of the kcp wasm instance (linear memory captured after
`/readyz`, restored in the browser instead of a cold boot) contains every secret
kcp generated during that boot:

- the serving and loopback CA and certificate private keys,
- the service-account signing key (`sa.key`),
- the admin, shard-admin and user bearer tokens (and the loopback client token),
- any socialweb vault state present at capture time.

If one snapshot is produced at build time and shipped to every visitor, all
visitors share these secrets. That is harmless only while no token, signed
service-account JWT or certificate ever leaves the tab. Once socialweb, OAuth,
ingress or any exposure makes the shard reachable or lets its credentials be
presented elsewhere, anyone with the published snapshot can mint tokens or
impersonate the shard.

Open options:

1. Per-user hibernate first: boot cold once in the visitor's browser, snapshot
   to OPFS, restore from there on later visits. No shared secrets.
   Implemented by `scripts/browser/kcp_hibernate.js`; the snapshot stays in
   the visitor's OPFS (or IndexedDB), unencrypted, next to the JSON state that
   already holds the same keys.
2. If a build-time image is shipped, rotate after restore before serving any
   request: regenerate the CA/serving keys, the service-account key and all
   bearer tokens, and invalidate tokens signed with the baked key.
3. Never capture a build-time snapshot after socialweb sign-in or with any
   vault, OAuth session or PKCE state present.

Until one of these is in place, do not publish a build-time snapshot.
