# Bailer

Docker volume backup tool. Backs up container volumes as compressed tar archives, with filtering and pre/post backup hooks.

## Bailer Config

| Flag          | Env Var        | Default    | Description                                           |
|---------------|----------------|------------|-------------------------------------------------------|
| `-backup-dir` | `BACKUP_DIR`   | `/backups` | Directory to store backups                            |
| `-mode`       | `MODE`         | `exclude`  | Volume filtering mode: `exclude` or `include`         |
| `-once`       | `ONCE`         | `false`    | Run a single backup pass and exit                     |
| `-dry-run`    | `DRY_RUN`      | `false`    | Show what would be backed up without taking action    |
| `-daemon`     | `DAEMON`       | `0`        | Run continuously, backing up at the given interval    |

## Backup config

Bailer is configured per-container using Docker labels.

| Label | Description |
|-------|-------------|
| `com.greboid.bailer.exclude` | Comma-separated list of mount destinations to exclude (used in `exclude` mode), use `*` for all |
| `com.greboid.bailer.include` | Comma-separated list of mount destinations to include (used in `include` mode), use `*` for all |
| `com.greboid.bailer.pre-backup-cmd` | Command to run inside the container before backing up |
| `com.greboid.bailer.post-backup-cmd` | Command to run inside the container after backing up |
| `com.greboid.bailer.helper-image` | Image for an ephemeral helper container created by Bailer |
| `com.greboid.bailer.helper-pre-cmd` | JSON argv array to run in the helper container before backing up |
| `com.greboid.bailer.helper-post-cmd` | JSON argv array to run in the helper container after backing up |

Volumes are matched by their destination path inside the container (e.g. `/data`). The Docker socket is always excluded.

Pre/post commands only run when the container is running. A failed pre-backup command (exec or helper) skips the container
for that pass; a failed post-backup command is logged without affecting the backup.

### Helper containers

The exec-based `pre-backup-cmd` / `post-backup-cmd` run inside the target container via `sh -c`, which fails for
minimal images with no shell. The helper labels instead run in an **ephemeral container that Bailer creates itself**,
intended for shell-less images that expose a single entrypoint (e.g. `sqlite3`, `bbolt`).

- The command value must be a **JSON argv array**, not a shell string — the commands are passed directly as the
  container entrypoint and never wrapped in `sh -c`.
- Helper containers run as root and mount all of the target container's volume and bind mounts read-write
  (excluding the Docker socket), so commands can write snapshot files into the volumes.
- The helper image is pulled automatically if it is not already present.
- The helper container is removed after each run.
- When `helper-pre-cmd` is set, `pre-backup-cmd` is ignored; when `helper-post-cmd` is set, `post-backup-cmd`
  is ignored. Only one mechanism runs per phase.
- A `helper-pre-cmd` or `helper-post-cmd` without `helper-image` is an error: the pre-command failure skips the
  container, the post-command failure is logged.

```yaml
labels:
  - 'com.greboid.bailer.helper-image=git.mouse-lake.ts.net/containers/sqlite3:latest'
  - 'com.greboid.bailer.helper-pre-cmd=["/usr/bin/sqlite3","-bail","-cmd",".timeout 5000","/data/app.db",".backup /data/app.db.bak"]'
```

When running Bailer as a container, you should exclude its backup mount to avoid backing up the backup directory.

## Backup Output

Backups are stored as `.tar.gz` files under the backup directory, organised by container name:

```
/backups/
  myapp/
    _data_20260409-143001.tar.gz
    _etc_myapp_20260409-143001.tar.gz
  database/
    _var_lib_postgresql_data_20260409-143001.tar.gz
```

## Docker Compose Example

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
