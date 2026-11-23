// csac-migrate reviews and imports a quiesced CsAC v1 backup.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"csac/v2/internal/legacy"

	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv); err != nil {
		fmt.Fprintln(os.Stderr, "csac-migrate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, env func(string) string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, "Usage: csac-migrate review [options] | csac-migrate import -review FILE [options]")
		fmt.Fprintln(stdout, "Use CSAC_LEGACY_DSN for read-only MariaDB and CSAC_DATABASE_URL for destination PostgreSQL.")
		return nil
	}
	command := args[0]
	if command != "review" && command != "import" {
		return errors.New("expected review or import")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	sourceMedia := flags.String("legacy-media-root", env("CSAC_LEGACY_MEDIA_ROOT"), "legacy snapshot root containing upload/, uploads/ and optionally emojis/")
	var opts legacy.Options
	var asOf, reviewPath, destMedia *string
	if command == "review" {
		flags.StringVar(&opts.WallTimeZone, "legacy-time-zone", legacy.DefaultWallTimeZone, "IANA zone used by v1 local DATETIME fields")
		flags.StringVar(&opts.MediaOrigin, "legacy-media-origin", "", "legacy HTTP(S) origin for absolute URLs in the local archive")
		flags.Int64Var(&opts.MaxMediaBytes, "max-media-bytes", 256<<20, "maximum bytes copied per file")
		asOf = flags.String("as-of", "", "RFC3339 cutoff for snapshot status and synthetic timestamps (default: current time)")
	} else {
		reviewPath = flags.String("review", "", "JSON review issued by the review subcommand")
		destMedia = flags.String("destination-media-root", env("CSAC_MEDIA_ROOT"), "new PostgreSQL-backed service's private media volume")
	}
	for _, arg := range args[1:] {
		if arg == "-h" || arg == "--help" {
			// Print schema defaults only, avoiding configured roots and secrets.
			flags.VisitAll(func(f *flag.Flag) { fmt.Fprintf(stdout, "  -%s\t%s\n", f.Name, f.Usage) })
			return nil
		}
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 {
		return errors.New("invalid options, see subcommand -h")
	}
	dsn := env("CSAC_LEGACY_DSN")
	if dsn == "" {
		return errors.New("CSAC_LEGACY_DSN is required")
	}
	if command == "review" {
		if *asOf != "" {
			parsed, err := time.Parse(time.RFC3339, *asOf)
			if err != nil {
				return errors.New("-as-of must use RFC3339 format")
			}
			opts.AsOf = parsed
		}
		plan, err := legacy.Prepare(ctx, dsn, *sourceMedia, opts)
		if err != nil {
			return err
		}
		r := plan.Report()
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(r); err != nil {
			return errors.New("cannot write JSON review")
		}
		if r.Blocked() {
			return errors.New("review contains blocking findings")
		}
		return nil
	}
	if *reviewPath == "" {
		return errors.New("-review is required")
	}
	file, err := os.Open(*reviewPath)
	if err != nil {
		return errors.New("cannot open review file")
	}
	defer file.Close()
	review, err := legacy.ReadReport(io.LimitReader(file, 2<<20))
	if err != nil {
		return err
	}
	if review.Blocked() {
		return errors.New("review contains blocking findings")
	}
	destination := env("CSAC_DATABASE_URL")
	if destination == "" {
		return errors.New("CSAC_DATABASE_URL is required")
	}
	plan, err := legacy.Prepare(ctx, dsn, *sourceMedia, review.Options)
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, destination)
	if err != nil {
		return errors.New("invalid PostgreSQL configuration")
	}
	defer pool.Close()
	if err := plan.Apply(ctx, pool, review, *destMedia); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Committed migration %s with %d accounts, %d conversations, %d messages, and %d media objects.\n",
		review.Fingerprint, review.Planned["accounts"], review.Planned["conversations"], review.Planned["messages"], review.Planned["media_objects"])
	return nil
}
