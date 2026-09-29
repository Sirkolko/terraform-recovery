package recovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/Sirkolko/terraform-recovery/internal/discovery"
	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// File names inside the state directory (.recovery by default).
const (
	mappingFileName   = "mapping.json"
	inventoryFileName = "inventory.json"
	lastPlanFileName  = "last-plan.json"
)

// store persists recovery progress in the state directory. Nothing is
// written when the session runs in dry-run mode.
type store struct {
	dir    string
	dryRun bool
}

func (s *store) path(name string) string { return filepath.Join(s.dir, name) }

// ensureDir creates the state directory with owner-only permissions and a
// .gitignore so its contents (which describe the infrastructure) are never
// committed by accident. The user's own .gitignore is not touched.
func (s *store) ensureDir() error {
	if s.dryRun {
		return errors.New("dry-run mode: nothing is written")
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	gi := filepath.Join(s.dir, ".gitignore")
	if _, err := os.Stat(gi); errors.Is(err, fs.ErrNotExist) {
		if err := os.WriteFile(gi, []byte("# Created by terraform-recovery: keep recovery data out of version control.\n*\n"), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) loadMapping() (*models.MappingFile, error) {
	data, err := os.ReadFile(s.path(mappingFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return models.NewMappingFile(), nil
	}
	if err != nil {
		return nil, err
	}
	m := models.NewMappingFile()
	if err := json.Unmarshal(data, m); err != nil {
		return nil, fmt.Errorf("invalid mapping file %s: %w", s.path(mappingFileName), err)
	}
	if m.Version > models.MappingFileVersion {
		return nil, fmt.Errorf("mapping file %s has unsupported version %d", s.path(mappingFileName), m.Version)
	}
	m.Normalize()
	return m, nil
}

func (s *store) saveMapping(m *models.MappingFile) error {
	if s.dryRun {
		return nil
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	m.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return discovery.WriteFileAtomic(s.path(mappingFileName), data, 0o600)
}

func (s *store) loadInventory() (*models.Inventory, error) {
	inv, err := discovery.LoadInventory(s.path(inventoryFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return inv, err
}

func (s *store) saveInventory(inv *models.Inventory) error {
	if s.dryRun {
		return nil
	}
	if err := s.ensureDir(); err != nil {
		return err
	}
	return discovery.SaveInventory(s.path(inventoryFileName), inv)
}

// newSnapshotDir creates a timestamped directory for one plan run.
func (s *store) newSnapshotDir(now time.Time) (string, string, error) {
	if err := s.ensureDir(); err != nil {
		return "", "", err
	}
	id := now.UTC().Format("20060102T150405Z") + "-" + newID()[:6]
	dir := filepath.Join(s.dir, id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return "", "", err
	}
	return id, dir, nil
}

func (s *store) saveJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return discovery.WriteFileAtomic(path, data, 0o600)
}

func (s *store) loadLastPlan() (*PlanRecord, error) {
	data, err := os.ReadFile(s.path(lastPlanFileName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p PlanRecord
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// backupFiles copies local files that Terraform may modify (a local state
// file and the dependency lock file) into the snapshot before anything runs.
func backupFiles(projectDir, snapshotDir string) ([]string, error) {
	var copied []string
	for _, name := range []string{"terraform.tfstate", "terraform.tfstate.backup", ".terraform.lock.hcl", RecoveryFile} {
		src := filepath.Join(projectDir, name)
		info, err := os.Lstat(src)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if err := os.MkdirAll(filepath.Join(snapshotDir, "backup"), 0o700); err != nil {
			return copied, err
		}
		if err := copyFile(src, filepath.Join(snapshotDir, "backup", name)); err != nil {
			return copied, err
		}
		copied = append(copied, name)
	}
	return copied, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
