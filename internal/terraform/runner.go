package terraform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Runner invokes the Terraform (or OpenTofu) CLI. Terraform remains the
// source of truth for every Terraform operation: this tool never edits
// state or talks to AWS APIs to change anything itself.
type Runner struct {
	Bin       string
	Dir       string
	VarFiles  []string
	Vars      []string
	Workspace string
	// ExtraEnv is appended to the inherited environment (e.g. AWS_PROFILE).
	ExtraEnv []string
}

// Version is the parsed output of `terraform version`.
type Version struct {
	Raw      string `json:"raw"`
	Major    int    `json:"major"`
	Minor    int    `json:"minor"`
	Patch    int    `json:"patch"`
	OpenTofu bool   `json:"opentofu"`
}

// SupportsImportBlocks reports whether import blocks are available
// (Terraform >= 1.5, OpenTofu >= 1.6).
func (v Version) SupportsImportBlocks() bool {
	if v.OpenTofu {
		return v.Major > 1 || (v.Major == 1 && v.Minor >= 6)
	}
	return v.Major > 1 || (v.Major == 1 && v.Minor >= 5)
}

// String formats the version for display.
func (v Version) String() string {
	name := "Terraform"
	if v.OpenTofu {
		name = "OpenTofu"
	}
	return name + " " + v.Raw
}

// FindBinary locates the terraform binary: an explicit path, then
// terraform, then tofu on PATH.
func FindBinary(explicit string) (string, error) {
	if explicit != "" {
		return exec.LookPath(explicit)
	}
	for _, name := range []string{"terraform", "tofu"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", errors.New("terraform CLI not found on PATH; install Terraform >= 1.5 or pass --terraform")
}

func (r *Runner) env() []string {
	env := os.Environ()
	env = append(env,
		"TF_IN_AUTOMATION=1",
		"TF_INPUT=0",
		"CHECKPOINT_DISABLE=1", // no version-check calls to HashiCorp
	)
	if r.Workspace != "" {
		env = append(env, "TF_WORKSPACE="+r.Workspace)
	}
	return append(env, r.ExtraEnv...)
}

func (r *Runner) command(ctx context.Context, stdout, stderr io.Writer, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.Bin, args...)
	cmd.Dir = r.Dir
	cmd.Env = r.env()
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	detachFromTerminal(cmd)
	// Interrupt instead of killing so Terraform can release locks and
	// persist state cleanly; kill only if it does not exit in time.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 2 * time.Minute
	return cmd
}

// displayArgs redacts -var values, which may contain secrets.
func displayArgs(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		if v, ok := strings.CutPrefix(a, "-var="); ok {
			name, _, _ := strings.Cut(v, "=")
			a = "-var=" + name + "=<redacted>"
		}
		out[i] = a
	}
	return strings.Join(out, " ")
}

// run executes terraform, streaming combined output to out, and returns the
// exit code.
func (r *Runner) run(ctx context.Context, out io.Writer, args ...string) (int, error) {
	fmt.Fprintf(out, "$ %s %s\n", filepath.Base(r.Bin), displayArgs(args))
	cmd := r.command(ctx, out, out, args...)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// output executes terraform and captures stdout and stderr separately.
func (r *Runner) output(ctx context.Context, args ...string) ([]byte, []byte, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := r.command(ctx, &stdout, &stderr, args...)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return nil, nil, -1, err
	}
	return stdout.Bytes(), stderr.Bytes(), 0, nil
}

func (r *Runner) varArgs() []string {
	var args []string
	for _, f := range r.VarFiles {
		args = append(args, "-var-file="+f)
	}
	for _, v := range r.Vars {
		args = append(args, "-var="+v)
	}
	return args
}

// Version returns the CLI version.
func (r *Runner) Version(ctx context.Context) (Version, error) {
	stdout, stderr, code, err := r.output(ctx, "version", "-json")
	if err != nil {
		return Version{}, err
	}
	if code != 0 {
		return Version{}, fmt.Errorf("terraform version failed: %s", strings.TrimSpace(string(stderr)))
	}
	var raw struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil || raw.Version == "" {
		return Version{}, fmt.Errorf("cannot parse terraform version output")
	}
	v := Version{Raw: raw.Version, OpenTofu: strings.Contains(filepath.Base(r.Bin), "tofu")}
	parts := strings.SplitN(strings.SplitN(raw.Version, "-", 2)[0], ".", 3)
	nums := []*int{&v.Major, &v.Minor, &v.Patch}
	for i := 0; i < len(parts) && i < 3; i++ {
		*nums[i], _ = strconv.Atoi(parts[i])
	}
	return v, nil
}

// Init runs terraform init. It never migrates or reconfigures state.
func (r *Runner) Init(ctx context.Context, out io.Writer) error {
	code, err := r.run(ctx, out, "init", "-input=false", "-no-color")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("terraform init failed (exit code %d)", code)
	}
	return nil
}

// Get downloads modules without initialising the backend.
func (r *Runner) Get(ctx context.Context, out io.Writer) error {
	code, err := r.run(ctx, out, "get", "-no-color")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("terraform get failed (exit code %d)", code)
	}
	return nil
}

// StateList returns the resource addresses currently in the state.
func (r *Runner) StateList(ctx context.Context) ([]string, error) {
	stdout, stderr, code, err := r.output(ctx, "state", "list")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		if strings.Contains(string(stderr), "No state file was found") {
			return nil, nil
		}
		return nil, fmt.Errorf("terraform state list failed: %s", strings.TrimSpace(string(stderr)))
	}
	var out []string
	for _, line := range strings.Split(string(stdout), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// StatePull returns the raw current state (possibly empty).
func (r *Runner) StatePull(ctx context.Context) ([]byte, error) {
	stdout, stderr, code, err := r.output(ctx, "state", "pull")
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("terraform state pull failed: %s", strings.TrimSpace(string(stderr)))
	}
	return stdout, nil
}

// ValidateResult is the result of terraform validate -json.
type ValidateResult struct {
	Valid       bool         `json:"valid"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

// Validate runs terraform validate.
func (r *Runner) Validate(ctx context.Context) (*ValidateResult, error) {
	stdout, stderr, _, err := r.output(ctx, "validate", "-json", "-no-color")
	if err != nil {
		return nil, err
	}
	var raw struct {
		Valid       bool `json:"valid"`
		Diagnostics []struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
			Detail   string `json:"detail"`
			Range    *struct {
				Filename string `json:"filename"`
				Start    struct {
					Line int `json:"line"`
				} `json:"start"`
			} `json:"range"`
		} `json:"diagnostics"`
	}
	if err := json.Unmarshal(stdout, &raw); err != nil {
		return nil, fmt.Errorf("cannot parse terraform validate output: %s", strings.TrimSpace(string(stderr)))
	}
	res := &ValidateResult{Valid: raw.Valid}
	for _, d := range raw.Diagnostics {
		diag := Diagnostic{Severity: d.Severity, Summary: d.Summary, Detail: d.Detail}
		if d.Range != nil {
			diag.File, diag.Line = d.Range.Filename, d.Range.Start.Line
		}
		res.Diagnostics = append(res.Diagnostics, diag)
	}
	return res, nil
}

// Plan runs terraform plan and saves the plan to planFile. The returned
// exit code follows -detailed-exitcode: 0 no changes, 1 error, 2 changes.
func (r *Runner) Plan(ctx context.Context, out io.Writer, planFile string, targets []string) (int, error) {
	args := []string{"plan", "-input=false", "-no-color", "-lock-timeout=60s", "-detailed-exitcode", "-out=" + planFile}
	args = append(args, r.varArgs()...)
	for _, t := range targets {
		args = append(args, "-target="+t)
	}
	return r.run(ctx, out, args...)
}

// ShowJSON returns the JSON representation of a saved plan.
func (r *Runner) ShowJSON(ctx context.Context, planFile string) ([]byte, error) {
	stdout, stderr, code, err := r.output(ctx, "show", "-json", "-no-color", planFile)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("terraform show failed: %s", strings.TrimSpace(string(stderr)))
	}
	return stdout, nil
}

// ShowText returns the human-readable rendering of a saved plan.
func (r *Runner) ShowText(ctx context.Context, planFile string) (string, error) {
	stdout, stderr, code, err := r.output(ctx, "show", "-no-color", planFile)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", fmt.Errorf("terraform show failed: %s", strings.TrimSpace(string(stderr)))
	}
	return string(stdout), nil
}

// Apply applies a saved plan. Terraform refuses to apply a plan whose state
// has changed since it was created, so exactly the reviewed plan is applied.
func (r *Runner) Apply(ctx context.Context, out io.Writer, planFile string) error {
	code, err := r.run(ctx, out, "apply", "-input=false", "-no-color", "-lock-timeout=60s", planFile)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("terraform apply failed (exit code %d)", code)
	}
	return nil
}
