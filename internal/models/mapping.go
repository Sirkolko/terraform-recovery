package models

import "time"

// Mapping sources describe how a mapping came to exist.
const (
	SourceSuggested   = "suggested"    // proposed by the matching engine, not yet confirmed
	SourceAccepted    = "accepted"     // suggestion confirmed by the user
	SourceManual      = "manual"       // linked by the user
	SourceManualID    = "manual-id"    // import ID typed by the user
	SourceDerived     = "derived"      // import ID computed from other mappings
	SourceImportBlock = "import-block" // import block already present in the configuration
	SourceState       = "state"        // address already present in the Terraform state
)

// MappingFileVersion is the current mapping file format version.
const MappingFileVersion = 1

// MappingFile is the persisted recovery progress. It only stores decisions
// made by the user; suggestions are recomputed deterministically.
type MappingFile struct {
	Version          int                      `json:"version"`
	Generator        string                   `json:"generator"`
	UpdatedAt        time.Time                `json:"updated_at"`
	AccountID        string                   `json:"account_id,omitempty"`
	Mappings         map[string]*MappingEntry `json:"mappings"`
	IgnoredTerraform map[string]*IgnoreEntry  `json:"ignored_terraform,omitempty"`
	IgnoredAWS       map[string]*IgnoreEntry  `json:"ignored_aws,omitempty"`
	// InstanceKeys lists instance keys supplied by the user for resources
	// whose count/for_each could not be evaluated statically, keyed by the
	// resource address without instance key.
	InstanceKeys map[string][]string `json:"instance_keys,omitempty"`
	// StateAddresses are the addresses found in the Terraform state the last
	// time it was checked (before planning or after applying).
	StateAddresses []string  `json:"state_addresses,omitempty"`
	StateCheckedAt time.Time `json:"state_checked_at,omitzero"`
}

// NewMappingFile returns an empty mapping file.
func NewMappingFile() *MappingFile {
	return &MappingFile{
		Version:          MappingFileVersion,
		Generator:        "terraform-recovery",
		Mappings:         map[string]*MappingEntry{},
		IgnoredTerraform: map[string]*IgnoreEntry{},
		IgnoredAWS:       map[string]*IgnoreEntry{},
		InstanceKeys:     map[string][]string{},
	}
}

// Normalize makes sure all maps are allocated.
func (m *MappingFile) Normalize() {
	if m.Mappings == nil {
		m.Mappings = map[string]*MappingEntry{}
	}
	if m.IgnoredTerraform == nil {
		m.IgnoredTerraform = map[string]*IgnoreEntry{}
	}
	if m.IgnoredAWS == nil {
		m.IgnoredAWS = map[string]*IgnoreEntry{}
	}
	if m.InstanceKeys == nil {
		m.InstanceKeys = map[string][]string{}
	}
	if m.Version == 0 {
		m.Version = MappingFileVersion
	}
	if m.Generator == "" {
		m.Generator = "terraform-recovery"
	}
}

// MappingEntry maps one Terraform resource address to an existing cloud
// resource.
type MappingEntry struct {
	Provider     string    `json:"provider"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	ImportID     string    `json:"import_id"`
	CloudKey     string    `json:"cloud_key,omitempty"`
	Region       string    `json:"region,omitempty"`
	Confidence   float64   `json:"confidence"`
	Confirmed    bool      `json:"confirmed"`
	Source       string    `json:"source"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// IgnoreEntry records that the user deliberately excluded a resource.
type IgnoreEntry struct {
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}
