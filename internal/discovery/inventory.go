package discovery

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// LoadInventory reads an inventory saved by SaveInventory or by the scan
// command, which allows reviewing mappings offline or on another machine.
func LoadInventory(path string) (*models.Inventory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var inv models.Inventory
	if err := json.Unmarshal(data, &inv); err != nil {
		return nil, fmt.Errorf("invalid inventory file %s: %w", path, err)
	}
	if inv.Version > models.InventoryVersion {
		return nil, fmt.Errorf("inventory file %s has unsupported version %d", path, inv.Version)
	}
	for _, r := range inv.Resources {
		if r.Tags == nil {
			r.Tags = map[string]string{}
		}
		if r.Attributes == nil {
			r.Attributes = map[string][]string{}
		}
		if r.Relations == nil {
			r.Relations = map[string][]string{}
		}
		if r.Identifiers == nil {
			r.Identifiers = map[string]string{}
		}
		if r.Key == "" {
			r.Key = models.CloudKey(r.Type, r.Region, r.ID)
		}
		if r.ImportID == "" {
			r.ImportID = r.ID
		}
	}
	Finalize(&inv)
	return &inv, nil
}

// SaveInventory writes an inventory atomically with owner-only permissions:
// it describes the infrastructure and should not be world-readable.
func SaveInventory(path string, inv *models.Inventory) error {
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, data, 0o600)
}

// WriteFileAtomic writes data to a temporary file and renames it into place.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
