# cvmfs_nfs_server

A read-only userspace NFSv3 server backed by libcvmfs.  It lets CernVM-FS
repositories be mounted through the operating system's native NFS client —
primarily for macOS, where this means no kernel extension, no macFUSE or
FUSE-T dependency, and mounts that do not require root.

```
cvmfs_nfs_server ── cgo ──> libcvmfs_client ── HTTP ──> stratum 1 / proxy
        │
        └─ NFSv3 on 127.0.0.1 <── mount_nfs (built-in macOS NFS client)
```

One daemon exports every configured repository as a top-level directory, so a
single mount at `/cvmfs` serves them all.  Repositories attach lazily on
first descent (listing `/cvmfs` alone attaches nothing), mirroring autofs
behavior on Linux.

## Design notes

**Persistent file handles.**  NFS clients cache file handles indefinitely and
present them again after reconnects — laptop sleep/wake, server restarts,
upgrades.  Handles here are 16 bytes (version byte, 7 reserved bytes, 64-bit
inode) where the inode comes from an append-only journal of `inode ↔ path`
mappings, in the spirit of the FUSE client's nfs_maps.  Mappings are
write-once; a path that disappears from the repository yields ENOENT rather
than a stale mount.  Restarting the daemon under a live mount is safe and has
been the core validation case.  Note the macOS NFS client silently rejects
file handles whose length is not a multiple of 4 — the symptom is an endless
ACCESS loop and EACCES on everything below the root.

**Presented metadata.**  Clients see the persistent inode (never the catalog
inode, which changes with catalog reloads) and ownership claimed as the
configured `-uid`/`-gid`, like the FUSE client's ownership claiming.

**Caches.**  libcvmfs file descriptors are cached per path (NFS READs are
stateless; without this every 32k READ would re-open the file), and directory
listings are cached briefly (READDIR re-lists per cookie page).  Both are
invalidated by a per-repository generation bumped on catalog refresh, which
runs every `-remount` (default 4m).  Client attribute caching (`acregmax`
etc.) bounds how quickly updates become visible, as with any NFS mount.

**Read-only by construction.**  All write-class billy operations return EROFS
and the NFS handler advertises no write capability; locking is unnecessary
(`nolocks`).

## Build

Requires a built libcvmfs (`-DBUILD_LIBCVMFS=ON`) and a Go toolchain with cgo.

```sh
BUILD=/path/to/build-libcvmfs/cvmfs        # contains libcvmfs_client.dylib
DEPS=/path/to/deps/lib                     # its @rpath deps (libssl, libcurl, ...)

CGO_ENABLED=1 \
CGO_LDFLAGS="-L$BUILD -lcvmfs_client -Wl,-rpath,$BUILD -Wl,-rpath,$DEPS" \
go build -o cvmfs_nfs_server .

go test .                                  # same CGO_LDFLAGS required to link
```

## Run

```sh
./cvmfs_nfs_server \
  -repos sft.cern.ch,lhcb.cern.ch \
  -keys /etc/cvmfs/keys/cern.ch \
  -cache /var/cache/cvmfs-nfs \
  -proxy 'http://squid.example.org:3128|DIRECT'
```

Options of note:

- `-keys` — a directory (`CVMFS_KEYS_DIR`) or colon-separated key files
  (`CVMFS_PUBLIC_KEY`).
- `-server-url` — `CVMFS_SERVER_URL` template; `@fqrn@` is substituted per
  repository.
- `-cvmfs-opt KEY=VALUE` — repeatable escape hatch for any client option
  (e.g. `-cvmfs-opt CVMFS_QUOTA_LIMIT=20000`).
- `-uid`/`-gid` — reported file ownership (defaults to the daemon's user).
- `-state` — handle journal location; keep it on local disk and back it by
  the same lifecycle as the cache.  Deleting it invalidates handles held by
  currently-mounted clients (they must remount), nothing worse.
- System defaults from `/etc/cvmfs` (if present) are honored via
  `cvmfs_options_parse_default`; flags and `-cvmfs-opt` override them.

Debugging: `CVMFSNFS_DEBUG=1` logs each filesystem operation;
`LOG_LEVEL=trace` additionally logs every NFS RPC (go-nfs).

## Mount

```sh
mkdir -p /tmp/cvmfs && \
mount_nfs -o ro,nolocks,nosuid,vers=3,tcp,port=11149,mountport=11149 \
  localhost:/ /tmp/cvmfs
```

No root is required on macOS.  Single-repository mounts also work:
`mount_nfs ... localhost:/sft.cern.ch /some/where`.

### Mounting at /cvmfs

The macOS system volume is sealed; create the mount point once via
`/etc/synthetic.conf` (the same mechanism `cvmfs_config setup` uses) and
reboot:

```
# /etc/synthetic.conf — the separator must be a TAB
cvmfs
```

Then `mount_nfs ... localhost:/ /cvmfs`.  Mounting at `/cvmfs` also makes
absolute symlinks between repositories (`/cvmfs/other.repo/...`) resolve
naturally on the client.

### launchd

`deploy/` contains a LaunchDaemon plist template for the server and a mount
helper that waits for the server and mounts `/cvmfs`.  Adjust paths, then:

```sh
sudo cp deploy/ch.cern.cvmfs.nfsserver.plist /Library/LaunchDaemons/
sudo launchctl load -w /Library/LaunchDaemons/ch.cern.cvmfs.nfsserver.plist
```

## Limitations

- No extended attributes: NFSv3 has none, so the CVMFS magic xattrs
  (`user.hash`, catalog counters, ...) are not visible.  Exposing them needs
  NFSv4 named attributes or a talk-socket side channel.
- The first access to a repository blocks on attach (seconds); subsequent
  access is cached.  A failed attach is retried with a 30s backoff.
- Apps reading the mount may need the macOS "Network Volumes" privacy
  permission (one-time per-app prompt), as with any network filesystem.
- The handle journal grows by roughly the path length per *distinct* path
  ever accessed (not per access).  Working sets of tens of thousands of paths
  cost a few MB.
- libcvmfs performs no cache quota management by default; size the cache
  volume accordingly or pass quota options via `-cvmfs-opt`.
