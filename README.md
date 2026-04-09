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

Volumes are matched by their destination path inside the container (e.g. `/data`). The Docker socket is always excluded.

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
