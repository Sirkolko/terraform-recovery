package recovery

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/Sirkolko/terraform-recovery/internal/matching"
	"github.com/Sirkolko/terraform-recovery/internal/models"
)

// Errors returned for invalid user actions.
var (
	ErrUnknownAddress = errors.New("unknown Terraform resource address")
	ErrUnknownCloud   = errors.New("unknown AWS resource")
)

func (s *Service) entryFor(r *models.TerraformResource, c *models.CloudResource, importID, source string, confidence int) *models.MappingEntry {
	e := &models.MappingEntry{
		Provider:     r.ProviderType(),
		ResourceType: r.Type,
		ImportID:     importID,
		ResourceID:   importID,
		Confidence:   float64(confidence) / 100,
		Confirmed:    true,
		Source:       source,
		UpdatedAt:    s.now().UTC(),
		Region:       r.Region,
	}
	if c != nil {
		e.ResourceID, e.CloudKey, e.Region = c.ID, c.Key, c.Region
	}
	return e
}

// Link confirms a mapping chosen by the user.
func (s *Service) Link(address, cloudKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findTF(address)
	if r == nil {
		return ErrUnknownAddress
	}
	c := s.findCloud(cloudKey)
	if c == nil {
		return ErrUnknownCloud
	}
	if r.Unexpanded {
		return errors.New("add the instance keys of this resource before linking it")
	}
	if !matching.Compatible(r.Type, c.Type) {
		return fmt.Errorf("%s cannot be imported from a %s", r.Type, strings.TrimPrefix(c.Type, "aws_"))
	}
	for addr, m := range s.mapping.Mappings {
		if addr != address && m.Confirmed && m.CloudKey == cloudKey {
			return fmt.Errorf("%s is already linked to %s; unlink it first", c.ID, addr)
		}
	}
	conf := 0
	if cand, err := s.result.ScorePair(address, cloudKey); err == nil {
		conf = cand.Confidence
	}
	source := models.SourceManual
	if a := s.result.Assignments[address]; a != nil && a.CloudKey == cloudKey && a.Source == models.SourceSuggested {
		source = models.SourceAccepted
	}
	s.mapping.Mappings[address] = s.entryFor(r, c, c.ImportID, source, conf)
	delete(s.mapping.IgnoredTerraform, address)
	delete(s.mapping.IgnoredAWS, cloudKey)
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.rematchLocked()
	return nil
}

// Accept confirms the suggested mappings of the given addresses.
func (s *Service) Accept(addresses []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, addr := range addresses {
		a := s.result.Assignments[addr]
		if a == nil || a.Confirmed || a.CloudKey == "" || a.Source != models.SourceSuggested {
			continue
		}
		r, c := s.findTF(addr), s.findCloud(a.CloudKey)
		if r == nil || c == nil {
			continue
		}
		s.mapping.Mappings[addr] = s.entryFor(r, c, c.ImportID, models.SourceAccepted, a.Confidence)
		n++
	}
	if n == 0 {
		return 0, nil
	}
	if err := s.saveLocked(); err != nil {
		return 0, err
	}
	s.rematchLocked()
	return n, nil
}

// AcceptAbove confirms every unambiguous suggestion with at least the given
// confidence. It repeats until nothing changes, because confirming parents
// raises the confidence of the resources that depend on them.
func (s *Service) AcceptAbove(threshold int) (int, error) {
	if threshold < matching.SuggestThreshold || threshold > 100 {
		return 0, fmt.Errorf("threshold must be between %d and 100", matching.SuggestThreshold)
	}
	total := 0
	for round := 0; round < 10; round++ {
		s.mu.Lock()
		var addrs []string
		for addr, a := range s.result.Assignments {
			if !a.Confirmed && !a.Ambiguous && a.Source == models.SourceSuggested && a.Confidence >= threshold {
				addrs = append(addrs, addr)
			}
		}
		s.mu.Unlock()
		if len(addrs) == 0 {
			break
		}
		n, err := s.Accept(addrs)
		total += n
		if err != nil || n == 0 {
			return total, err
		}
	}
	return total, nil
}

// Unlink removes a confirmed mapping or manual import ID.
func (s *Service) Unlink(address string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.mapping.Mappings[address]; !ok {
		return errors.New("this resource has no confirmed mapping to remove")
	}
	delete(s.mapping.Mappings, address)
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.rematchLocked()
	return nil
}

// ValidateImportID rejects IDs that cannot be valid import IDs.
func ValidateImportID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("the import ID is empty")
	}
	if len(id) > 2048 {
		return errors.New("the import ID is too long")
	}
	for _, r := range id {
		if unicode.IsControl(r) {
			return errors.New("the import ID contains control characters")
		}
	}
	return nil
}

// SetManualID records an import ID typed by the user, for resource types
// without automatic discovery.
func (s *Service) SetManualID(address, id string) error {
	id = strings.TrimSpace(id)
	if err := ValidateImportID(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findTF(address)
	if r == nil {
		return ErrUnknownAddress
	}
	if r.Unexpanded {
		return errors.New("add the instance keys of this resource before mapping it")
	}
	s.mapping.Mappings[address] = s.entryFor(r, nil, id, models.SourceManualID, 100)
	delete(s.mapping.IgnoredTerraform, address)
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.rematchLocked()
	return nil
}

// Ignore kinds.
const (
	KindTerraform = "terraform"
	KindAWS       = "aws"
)

// Ignore deliberately excludes a resource: a Terraform resource that will
// not be imported, or an AWS resource that stays unmanaged.
func (s *Service) Ignore(kind, key, reason string) error {
	reason = strings.TrimSpace(reason)
	if len(reason) > 500 {
		return errors.New("the reason is too long")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := &models.IgnoreEntry{Reason: reason, UpdatedAt: s.now().UTC()}
	switch kind {
	case KindTerraform:
		if s.findTF(key) == nil {
			return ErrUnknownAddress
		}
		delete(s.mapping.Mappings, key)
		s.mapping.IgnoredTerraform[key] = entry
	case KindAWS:
		if s.findCloud(key) == nil {
			return ErrUnknownCloud
		}
		for addr, m := range s.mapping.Mappings {
			if m.CloudKey == key {
				return fmt.Errorf("this resource is linked to %s; unlink it first", addr)
			}
		}
		s.mapping.IgnoredAWS[key] = entry
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.rematchLocked()
	return nil
}

// Unignore reverts Ignore.
func (s *Service) Unignore(kind, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case KindTerraform:
		delete(s.mapping.IgnoredTerraform, key)
	case KindAWS:
		delete(s.mapping.IgnoredAWS, key)
	default:
		return fmt.Errorf("unknown kind %q", kind)
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.rematchLocked()
	return nil
}

// SetInstanceKeys supplies the instance keys of a resource whose count or
// for_each could not be evaluated, then reloads the configuration.
func (s *Service) SetInstanceKeys(resource string, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.findTF(resource)
	if r == nil || !r.Unexpanded {
		if _, ok := s.mapping.InstanceKeys[resource]; !ok {
			return errors.New("instance keys can only be set for resources whose instances are unknown")
		}
	}
	var clean []string
	seen := map[string]bool{}
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		if len(k) > 256 || strings.ContainsFunc(k, unicode.IsControl) {
			return fmt.Errorf("invalid instance key %q", k)
		}
		seen[k] = true
		clean = append(clean, k)
	}
	if len(clean) == 0 {
		delete(s.mapping.InstanceKeys, resource)
	} else {
		s.mapping.InstanceKeys[resource] = clean
	}
	if err := s.saveLocked(); err != nil {
		return err
	}
	s.reloadLocked()
	return nil
}
