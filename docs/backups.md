# Backups to an S3 bucket

Off unless `--backup-bucket` is set; without it nothing in the lifecycle changes.

Point spritesd at any S3-compatible bucket and every sprite's disk and checkpoints are
backed up to it, incrementally, after each suspend:

```sh
./bin/spritesd \
  --backup-endpoint http://garage-s3:3900 --backup-bucket mini-sprites \
  --backup-region home-cloud --backup-credentials-file ~/.config/mini-sprites/backup.env
```

The credentials file holds `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` lines (with or
without `export`); with no file the environment is used.

**Recovery point: the last completed upload** — normally the sprite's last suspend, or at
worst `--backup-interval` (6h) ago for one that has been running without pause. That is the
guarantee, and it is a backup tier, not upstream's storage architecture: a cold wake still
reads the local disk, and waking on another machine means downloading the disk first.

- A suspend is the consistent point: the guest has synced and the VM is paused or gone. On a
  reflink volume the disk is cloned instantly under the lifecycle lock and uploaded from the
  clone, so a periodic backup of a *running* sprite is possible too (quiesce, pause, clone,
  resume — the same sequence a checkpoint uses). Without reflinks there is no cheap
  consistent point, so a running sprite is skipped until it next suspends.
- **A backup never delays a wake and never fails a suspend.** A sprite that is asked for
  mid-upload wakes normally; where the disk is being read in place, the upload is the one
  that gives way, within a fraction of a second and before any manifest is written, and is
  retried once the sprite is suspended again.
- Anything missed is caught up: every `--backup-interval`/10 (at most 5 minutes) spritesd
  looks for stopped sprites whose disk is newer than their recovery point — a backup that
  failed, was deferred, or was still queued when spritesd shut down — and retries with a
  backoff. Recovery points are read back from the bucket at startup, so a restart neither
  forgets them nor re-reads every disk.
- Uploads are incremental and deduplicated: files are cut into 4 MiB content-addressed
  chunks, sparse holes are skipped entirely, and checkpoints share almost every chunk with
  the disk they were cloned from. A second backup after a small change transfers roughly
  that change.
- Warm memory snapshots are **not** backed up: they are guest-RAM-sized and only valid on
  the machine that took them. A restored sprite starts cold, with its filesystem and
  checkpoints intact.
- `--backup-key-file` (32 bytes, `openssl rand -hex 32 > key`) turns on client-side
  AES-256-GCM, for the chunks and for everything that describes a sprite (manifests carry
  its record, environment included). Chunk IDs are keyed hashes of the plaintext, so dedup
  survives; the price is that someone who can read the bucket can tell that two chunks are
  equal, and how many sprites there are. **A key kept only
  on the machine you are protecting against losing is not a backup** — copy it somewhere
  else, or leave encryption off and trust the bucket.
- A sprite labelled `nobackup` is skipped. Everything else is included once a bucket is set.
- Failures are visible, never fatal: an unreachable bucket is logged and reported in
  `GET /v1/sprites/<name>` under a non-upstream `backup` field (`phase`, `last_backup_at`,
  `last_uploaded_bytes`, `last_backup_size_bytes`, `error`, `failures`). Suspends, wakes and
  everything else carry on, including starting up: a bucket that is down when spritesd
  starts is retried until it answers.

Losing the machine, and getting it back somewhere else:

```sh
spritesd backups list                          # what is in the bucket, and every manifest
spritesd restore --all                         # or: spritesd restore dev [--manifest <stamp>]
spritesd backups prune --dry-run               # then without --dry-run
spritesd backups forget scratch                # drop one sprite's backups now, whatever the retention
```

`restore` rebuilds machine directories in `--data` and must run with spritesd **stopped**. A
restored sprite gets an address that is free on the new host. Deleting a sprite leaves a
tombstone rather than removing its backup, so losing a machine and deleting a sprite do not
look the same; `prune` retires tombstoned sprites after `--backup-retention` (30 days) and
collects chunks nothing references any more. It is safe to run beside a live spritesd: it
announces itself in the bucket, backups stand aside until it has finished, and one that
was overtaken by it checks its chunks again before committing. All of these take the same
`--backup-*` flags as the daemon, before any sprite names.
