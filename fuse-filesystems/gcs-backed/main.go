// package main implements a Google Cloud Storage bucket as a FUSE filesystem.
//
// Pre-requisites: libfuse3-dev, gcc, and Application Default Credentials
// (gcloud auth application-default login). Build with -tags fuse3.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"github.com/winfsp/cgofuse/fuse"
)

// octalFlag parses permission bits such as 0644.
type octalFlag uint32

func (o *octalFlag) String() string { return fmt.Sprintf("%#o", uint32(*o)) }

func (o *octalFlag) Set(s string) error {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil || v > 0o7777 {
		return fmt.Errorf("invalid mode %q", s)
	}

	*o = octalFlag(v)

	return nil
}

func main() {
	var (
		bucket   = flag.String("bucket", "", "GCS bucket to mount (required)")
		debug    = flag.Bool("debug", false, "log every GCS call")
		fileMode = octalFlag(0o644)
		dirMode  = octalFlag(0o755)
		cfg      = Config{UID: uint32(os.Getuid()), GID: uint32(os.Getgid())}
	)

	flag.BoolVar(&cfg.ImplicitDirs, "implicit-dirs", true, "treat prefixes without a placeholder object as directories")
	flag.DurationVar(&cfg.StatTTL, "stat-ttl", 5*time.Second, "how long lookups (including misses) are cached; 0 disables")
	flag.IntVar(&cfg.RenameDirLimit, "rename-dir-limit", 1000, "largest directory (in objects) renamed server-side; larger ones get EXDEV")
	flag.Int64Var(&cfg.MaxStagedBytes, "max-staged-bytes", 512<<20, "largest file that can be written (staged in memory)")
	flag.BoolVar(&cfg.ReadOnly, "read-only", false, "mount read-only")
	flag.BoolVar(&cfg.ReadStreams, "read-streams", true, "stream sequential reads over one request instead of one request per read")
	flag.DurationVar(&cfg.OpTimeout, "op-timeout", 30*time.Second, "timeout for metadata and listing calls")
	flag.DurationVar(&cfg.IOTimeout, "io-timeout", 5*time.Minute, "timeout for uploads, downloads and stalled reads")
	flag.Var(&fileMode, "file-mode", "permissions for files without stored metadata")
	flag.Var(&dirMode, "dir-mode", "permissions for directories without stored metadata")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s --bucket NAME [flags] MOUNTPOINT [fuse options, e.g. -o attr_timeout=1]\n\n", os.Args[0])
		flag.PrintDefaults()
	}

	flag.Parse()

	if *bucket == "" || flag.NArg() < 1 {
		flag.Usage()
		os.Exit(2)
	}

	cfg.FileMode, cfg.DirMode = uint32(fileMode), uint32(dirMode)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	if err := run(*bucket, cfg, flag.Arg(0), flag.Args()[1:]); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func run(bucket string, cfg Config, mountpoint string, fuseOpts []string) error {
	ctx := context.Background()

	client, err := storage.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("creating GCS client (run `gcloud auth application-default login`?): %w", err)
	}

	defer client.Close()

	b := newCountingBucket(newGCSBucket(client.Bucket(bucket)))

	// Check access with objects.list, which roles/storage.objectViewer and
	// objectUser grant.
	cctx, cancel := context.WithTimeout(ctx, cfg.OpTimeout)
	_, err = b.List(cctx, "", "/", 1)

	cancel()

	if err != nil {
		return fmt.Errorf("cannot list gs://%s: %w", bucket, err)
	}

	fs := NewFileSystem(b, cfg)
	host := fuse.NewFileSystemHost(fs)
	host.SetCapReaddirPlus(true)

	opts := []string{"-o", "fsname=gs://" + bucket, "-o", "subtype=gcsfs"}
	if cfg.ReadOnly {
		opts = append(opts, "-o", "ro")
	}

	opts = append(opts, fuseOpts...)

	slog.Info("mounting", "bucket", bucket, "mountpoint", mountpoint)

	ok := host.Mount(mountpoint, opts)

	slog.Info("unmounted", "gcs_calls", b.Counts())

	if !ok {
		return fmt.Errorf("mount failed")
	}

	return nil
}
