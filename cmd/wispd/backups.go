package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/arugula-salad/wisp/internal/backup"
	"github.com/arugula-salad/wisp/internal/server"
	"github.com/arugula-salad/wisp/internal/store"
)

// The backup tier's two offline commands: `wispd restore`, which rebuilds
// machine directories from the bucket onto a fresh host, and `wispd backups`,
// which lists, forgets and garbage-collects what is up there. Both talk to the bucket
// directly; neither needs a running daemon, and restore must not have one.

// backupFlags registers the bucket flags on fs, shared by the daemon and both
// subcommands so that the same arguments work everywhere.
func backupFlags(fs *flag.FlagSet) func() server.BackupOptions {
	endpoint := fs.String("backup-endpoint", "", "S3 endpoint for the backup tier, e.g. http://garage-s3:3900")
	bucket := fs.String("backup-bucket", "", "S3 bucket for sprite backups (empty disables backups entirely)")
	region := fs.String("backup-region", "us-east-1", "S3 region the bucket reports, e.g. home-cloud for Garage")
	creds := fs.String("backup-credentials-file", "", "file of AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY lines (default: the environment)")
	key := fs.String("backup-key-file", "", "32-byte key (64 hex chars) enabling client-side encryption; a key kept only on this machine is not a backup unless you copy it elsewhere")
	parallel := fs.Int("backup-parallel", 4, "concurrent chunk transfers; each holds 4 MiB")
	rate := fs.Int64("backup-rate-limit", 0, "cap backup traffic in bytes/second (0 = unlimited)")
	interval := fs.Duration("backup-interval", 6*time.Hour, "re-upload a sprite whose disk changed this long after its last backup; also the retry for a failed one (0 = only on suspend)")
	retention := fs.Duration("backup-retention", 30*24*time.Hour, "how long a deleted sprite's backups are kept by `wispd backups prune`")
	keep := fs.Int("backup-keep", 0, "manifests to keep per sprite in `wispd backups prune` (0 = all)")
	return func() server.BackupOptions {
		return server.BackupOptions{Endpoint: *endpoint, Bucket: *bucket, Region: *region,
			CredentialsFile: *creds, KeyFile: *key, Parallel: *parallel, RateLimit: *rate,
			Interval: *interval, Retention: *retention, Keep: *keep}
	}
}

func openRepo(ctx context.Context, opts server.BackupOptions, log *slog.Logger) (*backup.Repo, error) {
	if opts.Bucket == "" {
		return nil, errors.New("--backup-bucket is required")
	}
	return backup.Open(ctx, backup.Config{Endpoint: opts.Endpoint, Bucket: opts.Bucket,
		Region: opts.Region, CredentialsFile: opts.CredentialsFile, KeyFile: opts.KeyFile,
		Parallel: opts.Parallel, RateLimit: opts.RateLimit, Log: log})
}

// runRestore rebuilds sprites from the bucket into a data directory.
func runRestore(args []string) int {
	fs := flag.NewFlagSet("wispd restore", flag.ExitOnError)
	data := fs.String("data", defaultDataDir(), "data directory to restore into")
	all := fs.Bool("all", false, "restore every sprite in the bucket that has not been deleted")
	manifest := fs.String("manifest", "", "restore this manifest instead of the newest (a stamp from `wispd backups list`)")
	force := fs.Bool("force", false, "replace a sprite that already exists in the data directory")
	rename := fs.String("rename", "", "restore under this name instead of the one in the backup")
	backupOpts := backupFlags(fs)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `Usage: wispd restore [flags] [sprite ...]

Rebuilds each named sprite's machine directory from the backup bucket. Stop
wispd first: it holds the data directory's sprite list in memory.

A restored sprite boots cold. Its filesystem and checkpoints come back; the warm
memory snapshot does not, because it is only valid on the machine that took it.

`)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Hour)
	defer cancel()

	repo, err := openRepo(ctx, backupOpts(), log)
	if err != nil {
		return fail(err)
	}
	abs, err := filepath.Abs(*data)
	if err != nil {
		return fail(err)
	}
	st, err := store.Open(abs)
	if err != nil {
		return fail(err)
	}

	wanted, err := chooseSprites(ctx, repo, fs.Args(), *all)
	if err != nil {
		return fail(err)
	}
	if *rename != "" && len(wanted) != 1 {
		return fail(errors.New("--rename takes exactly one sprite"))
	}

	var failures int
	for _, info := range wanted {
		if err := restoreOne(ctx, repo, st, info, *manifest, *rename, *force); err != nil {
			fmt.Fprintf(os.Stderr, "restore %s: %v\n", info.Name, err)
			failures++
			continue
		}
	}
	if failures > 0 {
		return 1
	}
	fmt.Printf("restored %d sprite(s) into %s\nStart wispd to use them.\n", len(wanted), abs)
	return 0
}

// chooseSprites resolves the command line to bucket entries.
func chooseSprites(ctx context.Context, repo *backup.Repo, names []string, all bool) ([]backup.SpriteInfo, error) {
	if all == (len(names) > 0) {
		return nil, errors.New("name the sprites to restore, or pass --all")
	}
	if all {
		every, err := repo.Sprites(ctx)
		if err != nil {
			return nil, err
		}
		var out []backup.SpriteInfo
		for _, s := range every {
			if s.Deleted != nil {
				fmt.Fprintf(os.Stderr, "skipping %s: deleted on %s (restore it by name to override)\n",
					s.Name, s.Deleted.DeletedAt.Format(time.RFC3339))
				continue
			}
			if s.Latest == nil {
				fmt.Fprintf(os.Stderr, "skipping %s: no complete backup\n", s.ID)
				continue
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil, errors.New("nothing to restore")
		}
		return out, nil
	}
	var out []backup.SpriteInfo
	for _, name := range names {
		info, others, err := repo.Find(ctx, name)
		if err != nil {
			return nil, err
		}
		if len(others) > 0 {
			fmt.Fprintf(os.Stderr, "note: %d older sprite(s) also went by %q; restoring the most recent (%s)\n",
				len(others), name, info.ID)
		}
		out = append(out, info)
	}
	return out, nil
}

func restoreOne(ctx context.Context, repo *backup.Repo, st *store.Store,
	info backup.SpriteInfo, stamp, rename string, force bool) error {
	m := info.Latest
	if stamp != "" {
		var err error
		if m, err = repo.LoadManifest(ctx, info.ID, stamp); err != nil {
			return err
		}
	}
	if m == nil {
		return errors.New("no complete backup to restore")
	}

	rec := backup.RestoreRecord(m)
	if rename != "" {
		rec.Name = rename
	}
	if existing, err := st.Get(rec.Name); err == nil {
		if !force {
			return fmt.Errorf("%q already exists here (id %s); pass --force to replace it, or --rename", rec.Name, existing.ID)
		}
		if err := st.Delete(rec.Name); err != nil {
			return err
		}
	}
	// Create reserves the name, allocates an address free on *this* host and makes
	// the machine directory; the record it writes is what the API will serve.
	if err := st.Create(&rec); err != nil {
		return err
	}
	dir := st.Dir(rec.ID)
	fmt.Printf("%s (%s) from %s, %s of data\n", rec.Name, rec.ID, m.CreatedAt.Format(time.RFC3339), humanBytes(m.Bytes()))
	stats, err := repo.Restore(ctx, m, dir, func(format string, a ...any) {
		fmt.Printf("  "+format+"\n", a...)
	})
	if err != nil {
		st.Delete(rec.Name) // removes the half-written directory too
		return err
	}
	fmt.Printf("  %d chunks, %s written in %s\n", stats.Chunks, humanBytes(stats.Read),
		stats.Took.Round(time.Millisecond))
	return nil
}

// runBackups is `wispd backups list|prune|forget`.
func runBackups(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: wispd backups list|prune|forget [flags] [sprite ...]")
		return 2
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("wispd backups "+sub, flag.ExitOnError)
	dryRun := fs.Bool("dry-run", false, "report what prune would delete, and delete nothing")
	grace := fs.Duration("grace", time.Hour, "prune leaves unreferenced chunks younger than this alone: they may belong to a backup that has not written its manifest yet")
	backupOpts := backupFlags(fs)
	fs.Parse(rest)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	opts := backupOpts()
	repo, err := openRepo(ctx, opts, log)
	if err != nil {
		return fail(err)
	}

	switch sub {
	case "list":
		sprites, err := repo.Sprites(ctx)
		if err != nil {
			return fail(err)
		}
		if len(sprites) == 0 {
			fmt.Printf("no backups in %s\n", opts.Bucket)
			return 0
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
		fmt.Fprintln(w, "NAME\tID\tSTATE\tLAST BACKUP\tSIZE\tMANIFESTS")
		for _, s := range sprites {
			state, last, size := "live", "-", "-"
			if s.Deleted != nil {
				state = "deleted " + s.Deleted.DeletedAt.Format("2006-01-02")
			}
			if s.Latest != nil {
				last = s.Latest.CreatedAt.Format(time.RFC3339)
				size = humanBytes(s.Latest.Bytes())
			}
			name := s.Name
			if name == "" {
				name = "(unknown)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\n", name, s.ID, state, last, size, len(s.Stamps))
		}
		w.Flush()
		for _, s := range sprites {
			if len(s.Stamps) > 1 {
				fmt.Printf("\n%s manifests:\n", s.Name)
				for _, stamp := range s.Stamps {
					fmt.Printf("  %s\n", stamp)
				}
			}
		}
		return 0

	case "prune":
		stats, err := repo.Prune(ctx, backup.PruneOptions{Retention: opts.Retention,
			Keep: opts.Keep, Grace: *grace, Settle: 5 * time.Second, DryRun: *dryRun}, func(format string, a ...any) {
			fmt.Printf(format+"\n", a...)
		})
		if err != nil {
			return fail(err)
		}
		verb := "deleted"
		if *dryRun {
			verb = "would delete"
		}
		fmt.Printf("%s %d chunks (%s), %d manifests, %d retired sprites; %d chunks still referenced\n",
			verb, stats.Chunks, humanBytes(stats.Bytes), stats.Manifests, stats.SpritesRetired, stats.ChunksKept)
		return 0

	case "forget":
		// The one command here that discards a recovery point on request, so it takes
		// names and nothing like --all. The chunks wait for the next prune.
		if fs.NArg() == 0 {
			return fail(errors.New("name the sprites whose backups to forget"))
		}
		for _, name := range fs.Args() {
			info, _, err := repo.Find(ctx, name)
			if err != nil {
				return fail(err)
			}
			n, err := repo.Forget(ctx, info.ID)
			if err != nil {
				return fail(err)
			}
			fmt.Printf("forgot %s (%s): %d manifests; run `wispd backups prune` to collect its chunks\n", info.Name, info.ID, n)
		}
		return 0
	}
	fmt.Fprintf(os.Stderr, "unknown subcommand %q (want list, prune or forget)\n", sub)
	return 2
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
