## Setting Up Fuse

1. Install the linux packages:

```bash

sudo apt install libfuse-dev libfuse3-dev gcc
```

2. Reload the gopls language server in VS Code

3. Optionally add these lines to .vscode/settings.json to build using fuse3

```json
{
  "go.buildFlags": ["-tags=fuse3"],
  "gopls": {
    "build.buildFlags": ["-tags=fuse3"]
  }
}
```

4. Running, from inside /fuse-filesystems:

```bash

go build -tags fuse3 -o mem.out ./in-memory

mkdir -p /tmp/memfs
./mem.out /tmp/memfs
```

On a second terminal:

```bash

mkdir /tmp/memfs/home
echo "home directory" > /tmp/memfs/home/data.txt
cat /tmp/memfs/home/data.txt
ls -la /tmp/memfs
```

## GCS-backed filesystem (`gcs-backed/`)

Mounts a Google Cloud Storage bucket. Each object is a file, and `/`-delimited
prefixes are directories. GCS stays the source of truth: the process caches
lookups for a few seconds, reads files with range requests, and stages writes in
memory until `close()` or `fsync()`, when it uploads the whole object.

### Try it without GCP

`fakegcs` runs an in-memory GCS emulator on localhost. From `fuse-filesystems/`:

```bash
go run ./gcs-backed/fakegcs -bucket lab &          # emulator on 127.0.0.1:4443
go build -tags fuse3 -o gcs.out ./gcs-backed

mkdir -p /tmp/gcsfs
STORAGE_EMULATOR_HOST=127.0.0.1:4443 ./gcs.out --bucket lab /tmp/gcsfs
```

Use it from a second terminal like the in-memory one. Ctrl+C, `kill`, or
`fusermount3 -u /tmp/gcsfs` unmounts it and saves any file still open.

### Use a real bucket

1. Credentials come from Application Default Credentials, so no key files are
   needed: `gcloud auth application-default login`.
2. Grant your identity `roles/storage.objectUser` **on the bucket only**, or
   `roles/storage.objectViewer` if you mount with `--read-only`.
3. Use a dedicated lab bucket with soft delete on. Mistakes like `rm -rf` then
   have an undo window.

```bash
./gcs.out --bucket my-lab-bucket /tmp/gcsfs -o attr_timeout=1,entry_timeout=1
```

Flags go before the mountpoint. FUSE `-o` options go after it.

| Flag | Default | Meaning |
|---|---|---|
| `--bucket` | (required) | Bucket to mount |
| `--read-only` | false | Refuse all changes (also mounts with `-o ro`) |
| `--implicit-dirs` | true | Treat prefixes without a `dir/` placeholder object as directories |
| `--stat-ttl` | 5s | How long lookups, including misses, are cached. Other clients' changes show up within this time. 0 disables the cache |
| `--rename-dir-limit` | 1000 | Largest directory (in objects) renamed server-side. Larger ones return `EXDEV`, and `mv` then copies through the mount |
| `--max-staged-bytes` | 512 MiB | Largest file that can be written. Writes are staged in memory |
| `--read-streams` | true | Stream sequential reads over one request |
| `--op-timeout` / `--io-timeout` | 30s / 5m | Per-call timeouts, so a dead network gives `EIO` instead of a hung process |
| `--file-mode` / `--dir-mode` | 0644 / 0755 | Permissions for objects without stored metadata |
| `--debug` | false | Log every GCS call (operation, object, latency, error) |

On unmount the log prints how many GCS calls of each type the session made.

### How files map to objects

- `/a/b.txt` is object `a/b.txt`. `mkdir /a` writes an empty placeholder object `a/`.
- Mode and mtime are stored in custom metadata (`gcsfs-mode`, `gcsfs-mtime`).
  Objects written by other tools get `--file-mode` and their upload time.
- Every file belongs to the mounting user. `chown` succeeds only when it changes nothing.
- Object names that can't be POSIX paths (`a//b`, a `..` segment, segments over
  255 bytes) are hidden and logged. If a file and a directory share a name, the
  directory wins.

### Differences from a local filesystem

- **Data reaches GCS on `close()` or `fsync()`, not on `write()`.** If the upload
  fails, `close()` returns `EIO`, and the change is lost when the file is
  released. Many programs ignore errors from `close()`. A crash loses anything
  not yet closed.
- **Every change re-uploads the whole object.** Appending a line to a 1 GiB file
  uploads 1 GiB. GCS also allows only about one write per second to the same
  object name, so tight rewrite loops slow down.
- **Concurrent writers don't overwrite each other.** If someone else changes an
  object after you open it, your upload is rejected (`EIO`) and their version stays.
- **An open file reads the version that existed when it was opened.** If another
  client replaces or deletes it, further reads fail with `EIO` instead of mixing
  versions.
- **Renames copy and then delete,** so they aren't atomic. A crash in between
  leaves both copies, but no data is lost.
- After `rm`, a process that still has the file open can keep reading only if
  it had written to it, because the staged copy is still in memory. Otherwise
  reads fail once the object is gone.
- No hard links, symlinks, xattrs, device files or FIFOs. atime isn't stored.

### Tests

```bash
go test -tags fuse3 -race ./gcs-backed/      # no network or credentials needed
./gcs-backed/e2e.sh my-lab-bucket            # real mount against a real bucket
STORAGE_EMULATOR_HOST=127.0.0.1:4443 ./gcs-backed/e2e.sh lab   # or against fakegcs
```

The unit tests drive the filesystem methods directly, without mounting. They
run against two fakes: fake-gcs-server through the real GCS client, and
`memBucket`, a hand-written fake. The second one exists because fake-gcs-server
ignores generation preconditions on copy and delete. `e2e.sh` mounts the
filesystem, runs about 20 checks (including a remount to prove the data
persisted), and deletes its test directory afterwards.
