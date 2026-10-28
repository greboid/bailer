# Bailer

Docker volume backup tool. Backs up container volumes with filtering and pre/post backup hooks, either as local compressed tar archives or as encrypted, deduplicated Restic snapshots on remote storage such as [BorgBase](https://www.borgbase.com).

## Bailer Config

| Flag          | Env Var        | Default    | Description                                           |
|---------------|----------------|------------|-------------------------------------------------------|
| `-backup-dir` | `BACKUP_DIR`   | `/backups` | Directory to store backups (local backend); restic `files` mode stages under `<backup-dir>/staging` |
| `-mode`       | `MODE`         | `exclude`  | Volume filtering mode: `exclude` or `include`         |
| `-once`       | `ONCE`         | `false`    | Run a single backup pass and exit                     |
| `-dry-run`    | `DRY_RUN`      | `false`    | Show what would be backed up without taking action    |
| `-daemon`     | `DAEMON`       | `0`        | Run continuously, backing up at the given interval    |
| `-backend`    | `BACKEND`      | `local`    | Backup backend: `local` or `restic`                   |
| `-restic-bin` | `RESTIC_BIN`   | `restic`   | Path to the restic binary                             |
| `-restic-host`| `RESTIC_HOST`  | hostname   | Host identity stored in restic snapshots              |
| `-restic-mode`| `RESTIC_MODE`  | `tar`      | Restic snapshot layout: `tar` (streamed archive) or `files` (individual files, staged under the backup directory) |

## Backup config

Bailer is configured per-container using Docker labels.

| Label | Description |
|-------|-------------|
| `com.greboid.bailer.exclude` | Comma-separated list of mount destinations to exclude (used in `exclude` mode), use `*` for all |
| `com.greboid.bailer.include` | Comma-separated list of mount destinations to include (used in `include` mode), use `*` for all |
| `com.greboid.bailer.pre-backup-cmd` | Command to run inside the container before backing up |
| `com.greboid.bailer.post-backup-cmd` | Command to run inside the container after backing up |
| `com.greboid.bailer.exec-user` | User (name or `UID[:GID]`) to run pre/post-backup commands as; defaults to the container's user |
| `com.greboid.bailer.helper-image` | Image for an ephemeral helper container created by Bailer |
| `com.greboid.bailer.helper-pre-cmd` | JSON argv array to run in the helper container before backing up |
| `com.greboid.bailer.helper-post-cmd` | JSON argv array to run in the helper container after backing up |

Volumes are matched by their destination path inside the container (e.g. `/data`). The Docker socket is always excluded.

Pre/post commands only run when the container is running. A failed pre-backup command (exec or helper) skips the container
for that pass and fails the run; a failed post-backup command is reported but does not affect the backup itself. A run
with any failure — skipped container, failed volume backup, failed post-hook — exits non-zero in `-once` mode and logs a
`Scheduled backup failed` entry in daemon mode. Failure errors include the command's captured output (exec output, or
the helper container's logs), trimmed to the last kilobyte.

### Helper containers

The exec-based `pre-backup-cmd` / `post-backup-cmd` run inside the target container via `sh -c`, which fails for
minimal images with no shell. The helper labels instead run in an **ephemeral container that Bailer creates itself**,
intended for shell-less images that expose a single entrypoint (e.g. `sqlite3`, `bbolt`).

- The command value must be a **JSON argv array**, not a shell string — the commands are passed directly as the
  container entrypoint and never wrapped in `sh -c`.
- Helper containers run as root and mount all of the target container's volume and bind mounts read-write
  (excluding the Docker socket), so commands can write snapshot files into the volumes. `exec-user` does not
  apply to helpers; use a `user:`-prefixed argv entry or a wrapper image if the helper must drop privileges.
- The helper image is pulled automatically if it is not already present.
- The helper container is removed after each run.
- When `helper-pre-cmd` is set, `pre-backup-cmd` is ignored; when `helper-post-cmd` is set, `post-backup-cmd`
  is ignored. Only one mechanism runs per phase.
- A `helper-pre-cmd` or `helper-post-cmd` without `helper-image` is an error: the pre-command failure skips the
  container and fails the run; the post-command failure is reported without affecting the backup.

```yaml
labels:
  - 'com.greboid.bailer.helper-image=git.mouse-lake.ts.net/containers/sqlite3:latest'
  - 'com.greboid.bailer.helper-pre-cmd=["/usr/bin/sqlite3","-bail","-cmd",".timeout 5000","/data/app.db",".backup /data/app.db.bak"]'
```

When running Bailer as a container, you should exclude its backup mount to avoid backing up the backup directory.

## Local backend

With the default `local` backend, backups are stored as `.tar.gz` files under the backup directory, organised by
container name. Archives are written to a temporary file and renamed into place only when complete, so a failed backup
never leaves a truncated archive behind:

```
/backups/
  myapp/
    _data_20260409-143001.tar.gz
    _etc_myapp_20260409-143001.tar.gz
  database/
    _var_lib_postgresql_data_20260409-143001.tar.gz
```

## Restic backend

Setting `BACKEND=restic` switches to encrypted, deduplicated backups in a
[Restic](https://restic.net) repository. Bailer orchestrates Docker (discovery, filtering, hooks) and streams each
volume directly into the `restic` binary — no intermediate archive files are created and no gzip is applied before
restic, which compresses and encrypts the stream itself.

Repository configuration uses the standard restic environment variables; Bailer adds no configuration format of its own:

| Env var | Description |
|---------|-------------|
| `RESTIC_REPOSITORY` | Repository location, e.g. BorgBase's `rest:https://...` endpoint or a local path |
| `RESTIC_REPOSITORY_FILE` | Alternative: file containing the repository location |
| `RESTIC_PASSWORD_FILE` | File containing the repository password (recommended with Docker secrets) |
| `RESTIC_PASSWORD` | Repository password directly (avoid where possible) |
| `RESTIC_PASSWORD_COMMAND` | Command printing the repository password |

Bailer validates the backend selection, the restic binary and the repository configuration **before** starting a
backup pass, but never initialises the repository or manages credentials. Create the repository once with
`restic init` yourself.

### Snapshot layouts

Two snapshot layouts are available via `RESTIC_MODE`; tags, `--host` and repository configuration are identical
for both, so filtering and retention work the same way.

**`tar` (default)** — each volume is streamed as a single uncompressed tar archive straight into the `restic`
binary via `--stdin-from-command`. No intermediate archive files are created and no gzip is applied before restic,
which compresses and encrypts the stream itself:

```bash
restic backup \
  --host <host> \
  --tag bailer \
  --tag container:myapp \
  --tag volume:appdata \
  --tag path:/data \
  --stdin-filename myapp_appdata__data.tar \
  --stdin-from-command \
  -- /bailer export-volume --container-id <id> --path /data
```

**`files`** (`RESTIC_MODE=files`) — each volume is extracted to a staging directory under the backup directory
(`<backup-dir>/staging/<mount>/<basename>`) and restic snapshots the individual files:

```bash
restic backup \
  --host <host> \
  --tag bailer \
  --tag container:myapp \
  --tag volume:appdata \
  --tag path:/data \
  /backups/staging/myapp_appdata__data/data
```

Restic cannot ingest a multi-file stream, so files mode always stages a full copy of the volume on disk first
(there is no way to untar on the fly into restic). Choosing between the modes:

- Files mode deduplicates per file: changing one file in a large volume only uploads that file's changed chunks,
  whereas a tar stream shifts every byte after the change and re-uploads nearly the whole volume. Files-mode
  snapshots are also browsable and restorable natively (`restic ls`, `restic restore`, `restic mount`).
- Files mode needs free disk space roughly equal to each volume, in the same filesystem as `-backup-dir` (point
  that at a real mounted filesystem when running as a container); the staged copy is I/O the tar mode does not pay.
- Files mode reads the volume twice over its lifetime (once to stage, once by restic); tar mode reads it once.
- The staging path is deterministic per mount, so restic finds the previous snapshot as its incremental parent and
  unchanged files are not re-read. The staged copy is removed after each run.
- Staging preserves permissions and modification times, and ownership when bailer runs as root; device nodes and
  FIFOs are skipped with a warning. Tar mode stores whatever Docker's export produces, including special files.
- In files mode the snapshot's top-level directory is the mount destination's base name (the same layout as inside
  the tar archives): exporting `/data` yields a snapshot containing `data/...`.

The `--host` value identifies the Docker host (configurable via `RESTIC_HOST`, defaulting to the hostname). Mount
identity is stable: the tar filename (or files-mode staging directory) is derived from the container name,
volume/mount identifier and mount destination — never from a timestamp — so the same mount maps to the same name
across backups. Container and volume names are **not** globally unique across hosts; keep one repository per host or
rely on `--host` for grouping and always filter snapshots by host.

Note that restic tags are for filtering only; they do not automatically define retention groups. Run `restic forget`
explicitly if you want retention, e.g. `restic forget --tag bailer --keep-daily 7 --keep-weekly 4`.

### BorgBase setup

1. Create a BorgBase account and add a **Restic repository** (choose an **EU storage location** if your data must
   remain in the EU).
2. Copy the repository URL shown by BorgBase — it looks like
   `rest:https://<user>.borgbase.com/repo/<id>` — this is your `RESTIC_REPOSITORY`.
3. Generate a strong password and store it in a secret file, e.g. `./secrets/restic_password`
   (`chmod 600`, no trailing newline issues: restic trims a single trailing newline).
4. Initialise the repository once:

   ```bash
   RESTIC_REPOSITORY='rest:https://<user>.borgbase.com/repo/<id>' \
     RESTIC_PASSWORD_FILE=./secrets/restic_password \
     restic init
   ```

5. Start Bailer with `BACKEND=restic` (compose example below). Verify the first run exits cleanly.
6. Inspect snapshots:

   ```bash
   RESTIC_REPOSITORY='rest:...' RESTIC_PASSWORD_FILE=./secrets/restic_password \
     restic snapshots --host <hostname>
   RESTIC_REPOSITORY='rest:...' RESTIC_PASSWORD_FILE=./secrets/restic_password \
     restic ls latest
   ```

### Docker Compose example (Restic + BorgBase)

```yaml
services:
  bailer:
    image: ghcr.io/greboid/bailer
    environment:
      BACKEND: restic
      ONCE: "true"
      RESTIC_REPOSITORY: ${RESTIC_REPOSITORY}
      RESTIC_PASSWORD_FILE: /run/secrets/restic_password
    secrets:
      - restic_password
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock

secrets:
  restic_password:
    file: ./secrets/restic_password
```

Add `DAEMON: 6h` instead of `ONCE: "true"` for scheduled operation. Passwords and repository URLs are never written
to Bailer's logs; restic's own output is logged line by line at debug level, and the last kilobyte is included in the
error when a backup fails.

### Docker Compose example (local backend)

```yaml
services:
  bailer:
    image: ghcr.io/greboid/bailer
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - ./backups:/backups
    environment:
      DAEMON: 6h
    labels:
      com.greboid.bailer.exclude: "/backups"
```

## Restoring data

Bailer does not modify production volumes during restore. Restores are manual, per snapshot:

1. On a machine with `restic` installed, configure repository credentials (the same `RESTIC_REPOSITORY` and
   `RESTIC_PASSWORD_FILE` as above).
2. List snapshots and pick one by ID:

   ```bash
   restic snapshots --host <hostname> --tag container:myapp
   ```

**Files-mode snapshots** (`RESTIC_MODE=files`) contain the individual files, so restic restores them directly. The
snapshot's top-level directory is the base name of the mount destination (exporting `/data` yields `data/...`), so
restore that subpath into a fresh volume:

   ```bash
   docker volume create myapp_data_restored
   RESTIC_REPOSITORY='rest:...' RESTIC_PASSWORD_FILE=./secrets/restic_password \
     restic restore <snapshot-id>:/data --target /var/lib/docker/volumes/myapp_data_restored/_data
   ```

   Restoring non-root ownership requires running the restore as root. Verify the contents as in steps 6–7 below.

**Tar-mode snapshots** store a single archive per volume, which is dumped and extracted manually:

3. Dump the stored tar stream to a file. The archive was created by Docker's
   [CopyFromContainer](https://docs.docker.com/reference/api/engine/#copy-files-or-folders-from-a-container),
   so its entries are rooted at the **base name of the mount destination**: exporting `/data` yields a top-level
   `data/` directory.

   ```bash
   restic dump <snapshot-id> myapp_appdata__data.tar > appdata.tar
   ```

4. Inspect the archive before extracting anything:

   ```bash
   tar -tvf appdata.tar
   ```

   Check the listing for absolute paths, `..` components, symlinks pointing outside the tree and special entries
   (devices, fifos). Docker's export never produces absolute paths or `..` entries, but verify before extracting
   anything you did not create yourself.

5. Create a fresh Docker volume and extract into it. Never extract directly over an existing production volume.

   ```bash
   docker volume create myapp_data_restored
   docker run --rm -i -v myapp_data_restored:/restore busybox \
     tar -xpf - -C /restore --strip-components=1 < appdata.tar
   ```

   - `-p` preserves permissions; `-C /restore --strip-components=1` drops the leading `data/` path component so the
     volume root matches the original mount contents. Omit `--strip-components=1` if you prefer the `data/` prefix.
   - For safety on untrusted archives, extract without `-p` first, review
     `tar -tvf appdata.tar`, and never extract as a step in a script that also touches production data.
   - Ownership is preserved by the tar entries; the extracting container runs as root (`busybox` defaults to root),
     which is required to restore non-root ownership.

6. Verify contents and metadata before switching over:

   ```bash
   docker run --rm -v myapp_data_restored:/data busybox \
     sh -c 'ls -laR /data; cat /data/hello.txt; readlink /data/link'
   ```

7. Point the application at the restored volume (new container, or stop the app, retarget the volume, start again).
   Never restore in place over a running container's data.

### Hook consistency caveat

Pre-backup hooks (exec or helper) provide only a short consistency window: Bailer runs the hook, then streams the
volume, which can take significant time for large volumes. Read consistency during streaming is not equivalent to an
atomic application snapshot. For databases, prefer helper-container `sqlite3 .backup` / `pg_dump`-style hooks writing
a consistent file into the volume, and accept that streaming adds delay between the hook and the snapshot.

## The export-volume subcommand

`bailer export-volume --container-id <id> --path /data` streams an uncompressed tar of the given container path to
stdout. It is an internal building block used by the tar mode of the restic backend and is not a general-purpose
export API: the path must be canonical and absolute, the container root filesystem is refused, hooks never run, and
diagnostics go only to stderr so the stdout stream is always pure tar data. It exits non-zero on any failure.

## Image contents

The production image contains the bailer binary and a pinned restic release built into
`ghcr.io/greboid/dockerbase/root` (which ships the `ca-certificates-bundle` trust store). The restic version is
pinned in the `Dockerfile` via `RESTIC_VERSION` with per-architecture SHA256 checksums from the official
`SHA256SUMS` release asset. To update: bump `RESTIC_VERSION` and refresh `RESTIC_SHA256_AMD64` /
`RESTIC_SHA256_ARM64` from the release's `SHA256SUMS`. No shell is installed in the runtime image.

## Development

```bash
go test ./...                                    # unit tests
```

## Known limitations

- Tags do not implement retention: schedule `restic forget`/`prune` yourself.
- Bailer never initialises repositories or manages credentials.
- Pre-backup hooks give a short consistency window only (see the caveat above).
- Restic snapshot identity depends on container names; recreating a container under a new name starts a new filename
  (old snapshots remain intact and tags still group them by volume).
- In restic `files` mode, staging needs free disk space roughly equal to each volume; special files (device nodes,
  FIFOs) are skipped, and ownership is only preserved when bailer runs as root.
