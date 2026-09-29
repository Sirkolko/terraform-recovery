// Command terraform-recovery helps recover Terraform state for existing AWS
// infrastructure: it maps Terraform resource addresses to discovered AWS
// resources, generates import blocks, runs terraform plan and — only after
// explicit approval of an import-only plan — terraform apply.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/Sirkolko/terraform-recovery/internal/discovery"
	"github.com/Sirkolko/terraform-recovery/internal/models"
	"github.com/Sirkolko/terraform-recovery/internal/recovery"
	"github.com/Sirkolko/terraform-recovery/internal/server"
)

// version is set at build time with -ldflags "-X main.version=...". Builds
// made with "go install ...@version" report the module version instead.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

// Exit codes.
const (
	exitOK      = 0
	exitError   = 1
	exitBlocked = 2 // the plan contains changes other than imports
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `terraform-recovery — recover Terraform state for existing AWS infrastructure

Usage:
  terraform-recovery [serve] [flags]     start the local web UI (default)
  terraform-recovery scan [flags]        discover AWS resources (read-only)
  terraform-recovery match [flags]       show suggested mappings
  terraform-recovery imports [flags]     print import blocks for confirmed mappings
  terraform-recovery plan [flags]        write recovery.import.tf and run terraform plan
  terraform-recovery apply [flags]       apply the last plan if it only imports
  terraform-recovery version

Common flags:
  --project DIR        Terraform root module (default ".")
  --profile NAME       AWS profile (default: SDK default credential chain)
  --region R           region to scan; repeat or comma-separate (default: from provider configuration)
  --var-file FILE      passed to evaluation and to terraform (repeatable)
  --var NAME=VALUE     passed to evaluation and to terraform (repeatable)
  --workspace NAME     Terraform workspace
  --terraform PATH     terraform or tofu binary (default: terraform, then tofu on PATH)
  --inventory FILE     use a saved inventory instead of scanning AWS
  --state-dir DIR      where recovery data is kept (default: <project>/.recovery)
  --dry-run            read-only: never write files or run terraform
  -v                   verbose logging

Run "terraform-recovery <command> -h" for command-specific flags.
`

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// varList keeps values intact (they may contain commas).
type varList []string

func (s *varList) String() string     { return strings.Join(*s, " ") }
func (s *varList) Set(v string) error { *s = append(*s, v); return nil }

type common struct {
	project, stateDir, profile, workspace, terraformBin, inventory string
	regions                                                        stringList
	varFiles, vars                                                 varList
	dryRun, verbose                                                bool
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.project, "project", ".", "Terraform root module directory")
	fs.StringVar(&c.stateDir, "state-dir", "", "recovery data directory (default <project>/.recovery)")
	fs.StringVar(&c.profile, "profile", discovery.DefaultProfile(), "AWS profile")
	fs.Var(&c.regions, "region", "AWS region(s) to scan")
	fs.Var(&c.varFiles, "var-file", "Terraform variable file (repeatable)")
	fs.Var(&c.vars, "var", "Terraform variable NAME=VALUE (repeatable)")
	fs.StringVar(&c.workspace, "workspace", "", "Terraform workspace")
	fs.StringVar(&c.terraformBin, "terraform", "", "terraform or tofu binary")
	fs.StringVar(&c.inventory, "inventory", "", "saved inventory file to use instead of scanning")
	fs.BoolVar(&c.dryRun, "dry-run", false, "read-only mode: write nothing, never run terraform")
	fs.BoolVar(&c.verbose, "v", false, "verbose logging")
}

func (c *common) logger(stderr io.Writer) *slog.Logger {
	level := slog.LevelWarn
	if c.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
}

func (c *common) service(ctx context.Context, stderr io.Writer) (*recovery.Service, error) {
	return recovery.New(ctx, recovery.Options{
		ProjectDir:    c.project,
		StateDir:      c.stateDir,
		Profile:       c.profile,
		Regions:       c.regions,
		VarFiles:      c.varFiles,
		Vars:          c.vars,
		Workspace:     c.workspace,
		TerraformBin:  c.terraformBin,
		InventoryFile: c.inventory,
		DryRun:        c.dryRun,
		Logger:        c.logger(stderr),
	})
}

func run(args []string, stdout, stderr io.Writer) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	code := exitOK
	switch cmd {
	case "serve":
		err = cmdServe(ctx, args, stdout, stderr)
	case "scan":
		err = cmdScan(ctx, args, stdout, stderr)
	case "match":
		err = cmdMatch(ctx, args, stdout, stderr)
	case "imports":
		err = cmdImports(ctx, args, stdout, stderr)
	case "plan":
		code, err = cmdPlan(ctx, args, stdout, stderr)
	case "apply":
		code, err = cmdApply(ctx, args, stdout, stderr)
	case "version":
		fmt.Fprintln(stdout, "terraform-recovery", version)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", cmd, usage)
		return exitError
	}
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return exitError
	}
	return code
}

func newFlagSet(name string, stderr io.Writer) (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	c := &common{}
	c.register(fs)
	return fs, c
}

// --- serve ---------------------------------------------------------------------

func cmdServe(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, c := newFlagSet("serve", stderr)
	listen := fs.String("listen", "127.0.0.1:0", "loopback address to listen on (a random free port by default)")
	open := fs.Bool("open", false, "open the UI in the default browser")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := c.service(ctx, stderr)
	if err != nil {
		return err
	}
	srv, err := server.New(svc, c.logger(stderr), version)
	if err != nil {
		return err
	}
	ln, err := srv.Listen(*listen)
	if err != nil {
		return err
	}
	url := srv.LoginURL()
	mode := "normal — writes only recovery.import.tf and .recovery/; apply needs explicit approval"
	if c.dryRun {
		mode = "dry run — read-only, nothing is written and terraform is never run"
	}
	fmt.Fprintf(stdout, "Terraform State Recovery Assistant %s\n\n", version)
	fmt.Fprintf(stdout, "  Project: %s\n", svc.ProjectDir())
	fmt.Fprintf(stdout, "  Mode:    %s\n", mode)
	fmt.Fprintf(stdout, "  Privacy: runs locally, no telemetry, no external services\n\n")
	fmt.Fprintf(stdout, "  Open this one-time login link in your browser (it stops working once used):\n\n    %s\n\n", url)
	if isTerminal(os.Stdin) {
		fmt.Fprintf(stdout, "Press Enter to print a new login link (for example for another browser). ")
		go printLoginLinks(os.Stdin, stdout, srv)
	}
	fmt.Fprintf(stdout, "Press Ctrl+C to stop.\n")
	if *open {
		openBrowser(url)
	}
	err = srv.Serve(ctx, ln)
	// A second Ctrl+C exits immediately. Terraform runs in its own process
	// group, so even then a running import apply is not interrupted.
	signal.Reset(os.Interrupt, syscall.SIGTERM)
	fmt.Fprintln(stdout, "\nStopping… running scans and plans are cancelled; a running import apply is allowed to finish (press Ctrl+C again to exit immediately).")
	svc.Close()
	return err
}

// printLoginLinks prints a new one-time login link whenever Enter is pressed.
func printLoginLinks(in io.Reader, out io.Writer, srv *server.Server) {
	lines := bufio.NewScanner(in)
	for lines.Scan() {
		fmt.Fprintf(out, "\n  New one-time login link:\n\n    %s\n\n", srv.NewLoginURL())
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if p, err := exec.LookPath("wslview"); err == nil {
			cmd = exec.Command(p, url)
		} else {
			cmd = exec.Command("xdg-open", url)
		}
	}
	_ = cmd.Start()
}

// --- scan ------------------------------------------------------------------------

func cmdScan(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, c := newFlagSet("scan", stderr)
	out := fs.String("out", "", "also write the inventory to this file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := c.service(ctx, stderr)
	if err != nil {
		return err
	}
	defer svc.Close()
	inv, err := svc.Scan(ctx, c.profile, c.regions, stderr)
	if err != nil {
		return err
	}
	if *out != "" {
		if err := discovery.SaveInventory(*out, inv); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Inventory written to %s\n", *out)
	}
	counts := map[string]int{}
	for _, r := range inv.Resources {
		counts[r.Type]++
	}
	var types []string
	for t := range counts {
		types = append(types, t)
	}
	sort.Strings(types)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "Account %s, regions %s\n\nTYPE\tCOUNT\n", inv.AccountID, strings.Join(inv.Regions, ", "))
	for _, t := range types {
		fmt.Fprintf(tw, "%s\t%d\n", t, counts[t])
	}
	tw.Flush()
	failed := 0
	for _, cov := range inv.Coverage {
		if cov.Status != models.CoverageOK {
			failed++
		}
	}
	if failed > 0 {
		fmt.Fprintf(stdout, "\n%d discovery call(s) failed; see the warnings above.\n", failed)
	}
	return nil
}

// --- match -----------------------------------------------------------------------

func cmdMatch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, c := newFlagSet("match", stderr)
	accept := fs.Int("accept", 0, "confirm unambiguous suggestions with at least this confidence (for example 95)")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := c.service(ctx, stderr)
	if err != nil {
		return err
	}
	defer svc.Close()
	if *accept > 0 {
		if c.dryRun {
			fmt.Fprintln(stderr, "Dry run: confirmations are not saved.")
		}
		n, err := svc.AcceptAbove(*accept)
		if err != nil {
			return err
		}
		fmt.Fprintf(stderr, "Confirmed %d suggestion(s) with confidence >= %d%%.\n", n, *accept)
	}
	v := svc.View()
	if v.AWS.AccountID == "" {
		fmt.Fprintln(stderr, "No inventory yet: run \"terraform-recovery scan\" or pass --inventory.")
	}
	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"summary": v.Summary, "terraform": v.Terraform, "cloud": v.Cloud})
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCONF\tTERRAFORM ADDRESS\tAWS RESOURCE / IMPORT ID\tSOURCE")
	for _, r := range v.Terraform {
		conf, target, source := "", "", ""
		if m := r.Mapping; m != nil {
			if m.Source == models.SourceSuggested || m.Source == models.SourceAccepted || m.Source == models.SourceManual {
				conf = fmt.Sprintf("%d%%", m.Confidence)
			}
			target = m.ImportID
			if m.CloudName != "" && m.CloudName != m.CloudID {
				target += " (" + m.CloudName + ")"
			}
			source = m.Source
			if m.Ambiguous {
				source += ", ambiguous"
			}
		} else if r.IgnoreReason != "" {
			target = "ignored: " + r.IgnoreReason
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Status, conf, r.Address, target, source)
	}
	tw.Flush()
	s := v.Summary
	fmt.Fprintf(stdout, "\nTerraform resources: %d  matched: %d  needs review: %d  unmatched: %d  ignored: %d\n",
		s.Terraform, s.Matched, s.Review, s.Unmatched, s.Ignored)
	fmt.Fprintf(stdout, "AWS resources: %d  unmanaged: %d  ignored: %d\n", s.Cloud, s.Unmanaged, s.CloudIgnored)
	if s.Unmanaged > 0 {
		fmt.Fprintln(stdout, "\nUnmanaged AWS resources:")
		for _, cv := range v.Cloud {
			if cv.Status == models.StatusUnmatched {
				fmt.Fprintf(stdout, "  %s %s %s\n", cv.Type, cv.ID, cv.Name)
			}
		}
	}
	for _, d := range v.Diagnostics {
		fmt.Fprintf(stderr, "%s: %s %s\n", d.Severity, d.Summary, d.Detail)
	}
	return nil
}

// --- imports ---------------------------------------------------------------------

func cmdImports(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs, c := newFlagSet("imports", stderr)
	out := fs.String("out", "", "write to this file instead of stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	svc, err := c.service(ctx, stderr)
	if err != nil {
		return err
	}
	defer svc.Close()
	p, err := svc.Preview()
	if err != nil {
		return err
	}
	for _, e := range p.Excluded {
		fmt.Fprintf(stderr, "excluded %s: %s\n", e.Address, e.Reason)
	}
	if p.HCL == "" {
		return errors.New("no confirmed mappings to import")
	}
	if *out == "" {
		_, err = io.WriteString(stdout, p.HCL)
		return err
	}
	if err := os.WriteFile(*out, []byte(p.HCL), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Wrote %d import block(s) to %s\n", len(p.Specs), *out)
	return nil
}

// --- plan and apply ----------------------------------------------------------------

func printPlan(w io.Writer, rec *recovery.PlanRecord) {
	fmt.Fprintf(w, "\nPlan %s (snapshot %s)\n", rec.ID, rec.Dir)
	if a := rec.Analysis; a != nil {
		fmt.Fprintf(w, "  %d to import, %d to add, %d to change, %d to destroy\n", a.Import, a.Add, a.Change, a.Destroy)
		for _, r := range a.Resources {
			if r.Action != "import" {
				fmt.Fprintf(w, "  %-16s %s %s\n", r.Action, r.Address, strings.Join(r.Changed, ", "))
			}
		}
	}
	if ok, why := rec.CanApply(); ok {
		fmt.Fprintln(w, "\nImport-only plan: nothing will be created, changed or destroyed.")
		fmt.Fprintf(w, "Apply it with: terraform-recovery apply --plan %s\n", rec.ID)
	} else {
		fmt.Fprintln(w, "\n"+why)
	}
}

func cmdPlan(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs, c := newFlagSet("plan", stderr)
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	svc, err := c.service(ctx, stderr)
	if err != nil {
		return exitError, err
	}
	defer svc.Close()
	rec, err := svc.Plan(ctx, stderr)
	if rec != nil {
		printPlan(stdout, rec)
	}
	if err != nil {
		return exitError, err
	}
	if ok, _ := rec.CanApply(); !ok {
		return exitBlocked, nil
	}
	return exitOK, nil
}

func cmdApply(ctx context.Context, args []string, stdout, stderr io.Writer) (int, error) {
	fs, c := newFlagSet("apply", stderr)
	planID := fs.String("plan", "", "ID of the plan to apply (default: the latest plan)")
	confirm := fs.String("confirm-plan", "", "non-interactive confirmation: the ID of the plan to apply")
	if err := fs.Parse(args); err != nil {
		return exitError, err
	}
	svc, err := c.service(ctx, stderr)
	if err != nil {
		return exitError, err
	}
	defer svc.Close()
	v := svc.View()
	if v.Plan == nil {
		return exitError, errors.New(`no plan yet: run "terraform-recovery plan" first`)
	}
	rec := v.Plan.PlanRecord
	if *planID != "" && *planID != rec.ID {
		return exitError, fmt.Errorf("plan %s is not the latest plan (%s); run plan again", *planID, rec.ID)
	}
	printPlan(stdout, rec)
	if !v.Plan.CanApply {
		return exitBlocked, errors.New("import has NOT been applied: " + v.Plan.BlockReason)
	}
	switch {
	case *confirm != "":
		if *confirm != rec.ID {
			return exitError, errors.New("--confirm-plan does not match the plan ID")
		}
	case isTerminal(os.Stdin):
		fmt.Fprintf(stdout, "\nTerraform will record %d existing resource(s) in the state. No infrastructure will change.\n", rec.Analysis.Import)
		fmt.Fprintf(stdout, "Type the plan ID (%s) to apply: ", rec.ID)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != rec.ID {
			return exitError, errors.New("not confirmed; nothing was applied")
		}
	default:
		return exitError, errors.New("stdin is not a terminal: pass --confirm-plan <plan ID> to confirm")
	}
	res, err := svc.Apply(ctx, rec.ID, true, stderr)
	if err != nil {
		return exitError, err
	}
	fmt.Fprintf(stdout, "\nState recovered: %d resource(s) imported.\n", len(res.Apply.Imported))
	if res.Excluded > 0 {
		fmt.Fprintf(stdout, "\nWARNING: recovery is NOT complete. %d Terraform resource(s) were left out of this import,\n", res.Excluded)
		fmt.Fprintln(stdout, "and a normal terraform plan will propose to create them. Map or resolve them before using")
		fmt.Fprintln(stdout, "this configuration normally.")
	}
	fmt.Fprintln(stdout, "Run a full terraform plan (without -target) to confirm there are no remaining changes. recovery.import.tf can now be deleted.")
	return exitOK, nil
}

// isTerminal reports whether f is an interactive terminal. /dev/null is a
// character device too, so it is excluded explicitly.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(info, null) {
		return false
	}
	return true
}
