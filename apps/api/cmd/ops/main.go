// Command sdano-ops is the platform operator CLI (docs/12-platform-ops.md):
// tenant lifecycle management run over SSH by the one person operating
// Sdano as a business. Flag-parsing and printing only — every mutation
// goes through internal/platform, which owns the transactional writes and
// the ops_audit trail.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/platform"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches to the tenant subcommands and returns the process exit
// code, so main itself stays a one-liner.
func run(args []string) int {
	if len(args) < 1 || args[0] != "tenant" {
		printUsage()
		return 1
	}
	if len(args) < 2 {
		printTenantUsage()
		return 1
	}

	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		fmt.Fprintln(os.Stderr, "sdano-ops: DATABASE_URL is required")
		return 1
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "sdano-ops: connecting to database: %v\n", err)
		return 1
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "sdano-ops: pinging database: %v\n", err)
		return 1
	}

	sub, rest := args[1], args[2:]
	var cmdErr error
	switch sub {
	case "create":
		cmdErr = cmdTenantCreate(ctx, pool, rest)
	case "list":
		cmdErr = cmdTenantList(ctx, pool, rest)
	case "suspend":
		cmdErr = cmdTenantSuspend(ctx, pool, rest)
	case "activate":
		cmdErr = cmdTenantActivate(ctx, pool, rest)
	case "set-billing":
		cmdErr = cmdTenantSetBilling(ctx, pool, rest)
	default:
		printTenantUsage()
		return 1
	}
	if cmdErr != nil {
		fmt.Fprintln(os.Stderr, "sdano-ops:", cmdErr)
		return 1
	}
	return 0
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage: sdano-ops tenant <create|list|suspend|activate|set-billing> [flags]")
}

func printTenantUsage() {
	fmt.Fprintln(os.Stderr, "usage: sdano-ops tenant <create|list|suspend|activate|set-billing> [flags]")
	fmt.Fprintln(os.Stderr, "  create      --name NAME [--trial-days N]")
	fmt.Fprintln(os.Stderr, "  list")
	fmt.Fprintln(os.Stderr, "  suspend     --id UUID [--note TEXT]")
	fmt.Fprintln(os.Stderr, "  activate    --id UUID")
	fmt.Fprintln(os.Stderr, "  set-billing --id UUID --billed-until YYYY-MM-DD [--plan-note TEXT]")
}

// newFlagSet builds a ContinueOnError flag set so a parse failure prints
// usage (flag's default behavior on error) and lets run() control the exit
// code, rather than flag.ExitOnError's os.Exit(2).
func newFlagSet(name, usageLine string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: sdano-ops tenant "+usageLine)
		fs.PrintDefaults()
	}
	return fs
}

func cmdTenantCreate(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := newFlagSet("create", "create --name NAME [--trial-days N]")
	name := fs.String("name", "", "tenant name (required)")
	trialDays := fs.Int("trial-days", 30, "trial length in days")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		fs.Usage()
		return errors.New("--name is required")
	}

	result, err := platform.OpsCreateTenant(ctx, pool, *name, *trialDays)
	if err != nil {
		return fmt.Errorf("creating tenant: %w", err)
	}

	fmt.Println("Tenant created.")
	fmt.Println("  Tenant ID:      ", result.TenantID)
	fmt.Println("  Admin email:    ", result.AdminEmail)
	fmt.Println("  Admin password: ", result.AdminPassword)
	fmt.Println()
	fmt.Println("This password is shown once and is not stored in plaintext anywhere. Save it now.")
	return nil
}

func cmdTenantList(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := newFlagSet("list", "list")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rows, err := platform.OpsListTenants(ctx, pool)
	if err != nil {
		return fmt.Errorf("listing tenants: %w", err)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tNAME\tSTATUS\tWORKERS\tOBJECTS\tTRIAL ENDS\tBILLED UNTIL\tSUSPENDED AT"); err != nil {
		return fmt.Errorf("writing table header: %w", err)
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
			r.ID, r.Name, r.Status, r.ActiveWorkers, r.ActiveObjects,
			formatTimestamptz(r.TrialEndsAt), formatDate(r.BilledUntil), formatTimestamptz(r.SuspendedAt)); err != nil {
			return fmt.Errorf("writing table row: %w", err)
		}
	}
	return w.Flush()
}

func cmdTenantSuspend(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := newFlagSet("suspend", "suspend --id UUID [--note TEXT]")
	id := fs.String("id", "", "tenant id (required)")
	note := fs.String("note", "", "reason for suspension")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		fs.Usage()
		return errors.New("--id is required")
	}
	tenantID, err := uuid.Parse(*id)
	if err != nil {
		fs.Usage()
		return fmt.Errorf("invalid --id: %w", err)
	}

	if err := platform.OpsSuspend(ctx, pool, tenantID, *note); err != nil {
		return fmt.Errorf("suspending tenant: %w", err)
	}
	fmt.Println("Tenant suspended.")
	return nil
}

func cmdTenantActivate(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := newFlagSet("activate", "activate --id UUID")
	id := fs.String("id", "", "tenant id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		fs.Usage()
		return errors.New("--id is required")
	}
	tenantID, err := uuid.Parse(*id)
	if err != nil {
		fs.Usage()
		return fmt.Errorf("invalid --id: %w", err)
	}

	if err := platform.OpsActivate(ctx, pool, tenantID); err != nil {
		return fmt.Errorf("activating tenant: %w", err)
	}
	fmt.Println("Tenant activated.")
	return nil
}

func cmdTenantSetBilling(ctx context.Context, pool *pgxpool.Pool, args []string) error {
	fs := newFlagSet("set-billing", "set-billing --id UUID --billed-until YYYY-MM-DD [--plan-note TEXT]")
	id := fs.String("id", "", "tenant id (required)")
	billedUntilStr := fs.String("billed-until", "", "billed-until date, YYYY-MM-DD (required)")
	planNote := fs.String("plan-note", "", "plan note (optional; empty keeps the existing note)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *billedUntilStr == "" {
		fs.Usage()
		return errors.New("--id and --billed-until are required")
	}
	tenantID, err := uuid.Parse(*id)
	if err != nil {
		fs.Usage()
		return fmt.Errorf("invalid --id: %w", err)
	}
	billedUntil, err := time.Parse("2006-01-02", *billedUntilStr)
	if err != nil {
		fs.Usage()
		return fmt.Errorf("invalid --billed-until (want YYYY-MM-DD): %w", err)
	}

	if err := platform.OpsSetBilling(ctx, pool, tenantID, billedUntil, *planNote); err != nil {
		return fmt.Errorf("setting billing: %w", err)
	}
	fmt.Println("Billing updated.")
	return nil
}

func formatTimestamptz(t pgtype.Timestamptz) string {
	if !t.Valid {
		return "-"
	}
	return t.Time.Format("2006-01-02 15:04")
}

func formatDate(d pgtype.Date) string {
	if !d.Valid {
		return "-"
	}
	return d.Time.Format("2006-01-02")
}
