//go:build integration

package recovery

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRealTerraform runs the plan and apply workflow against the real
// Terraform CLI using the hashicorp/random provider, which supports import
// and needs no cloud credentials. Run with:
//
//	go test -tags integration ./internal/recovery -run TestRealTerraform -v
//
// It needs terraform on PATH and network access to download the provider.
func TestRealTerraform(t *testing.T) {
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform not installed")
	}
	dir := t.TempDir()
	config := `
terraform {
  required_providers {
    random = { source = "hashicorp/random" }
  }
}

resource "random_uuid" "a" {}
resource "random_uuid" "b" {}

# random_pet cannot be imported: it is ignored, so the plan must be targeted
# and must not propose to create it.
resource "random_pet" "c" {}
`
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := New(ctx, Options{
		ProjectDir: dir,
		NewDiscoverer: func(context.Context, string) (Discoverer, error) {
			return fakeDiscoverer{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.tfErr != nil {
		t.Fatal(s.tfErr)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.SetManualID("random_uuid.a", "8e4b2c1a-7d3f-4e5a-9b6c-0d1e2f3a4b5c"))
	must(s.SetManualID("random_uuid.b", "1f2e3d4c-5b6a-4987-8765-4321fedcba98"))
	must(s.Ignore(KindTerraform, "random_pet.c", "cannot be imported"))

	rec, err := s.Plan(ctx, testWriter{t})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !rec.Targeted {
		t.Error("plan should be targeted because random_pet.c is excluded")
	}
	a := rec.Analysis
	if a.Import != 2 || a.Add != 0 || a.Change != 0 || a.Destroy != 0 {
		t.Fatalf("unexpected plan analysis: %+v", a)
	}
	if ok, why := rec.CanApply(); !ok {
		t.Fatalf("expected an import-only plan: %s", why)
	}

	rec, err = s.Apply(ctx, rec.ID, true, testWriter{t})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(rec.Apply.Imported) != 2 || len(rec.Apply.Missing) != 0 {
		t.Fatalf("apply result: %+v", rec.Apply)
	}
	if _, err := os.Stat(filepath.Join(dir, "terraform.tfstate")); err != nil {
		t.Errorf("state file not written: %v", err)
	}
	v := s.View()
	if v.Summary.InState != 2 {
		t.Errorf("in state = %d", v.Summary.InState)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}

var _ io.Writer = testWriter{}
