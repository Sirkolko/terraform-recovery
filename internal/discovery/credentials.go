package discovery

import (
	"bufio"
	"os"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
)

// Profiles lists the profile names defined in the shared AWS config and
// credentials files. Only section headers are inspected; key values —
// including any credentials — are skipped line by line and never retained.
func Profiles() []string {
	seen := map[string]bool{}
	configFile := os.Getenv("AWS_CONFIG_FILE")
	if configFile == "" {
		configFile = config.DefaultSharedConfigFilename()
	}
	credsFile := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credsFile == "" {
		credsFile = config.DefaultSharedCredentialsFilename()
	}
	for _, name := range sectionNames(configFile, true) {
		seen[name] = true
	}
	for _, name := range sectionNames(credsFile, false) {
		seen[name] = true
	}
	var out []string
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// DefaultProfile returns the profile selected through the environment.
func DefaultProfile() string {
	if p := os.Getenv("AWS_PROFILE"); p != "" {
		return p
	}
	return os.Getenv("AWS_DEFAULT_PROFILE")
}

func sectionNames(path string, isConfig bool) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
			continue
		}
		name := strings.TrimSpace(line[1 : len(line)-1])
		if isConfig {
			switch {
			case name == "default":
			case strings.HasPrefix(name, "profile "):
				name = strings.TrimSpace(strings.TrimPrefix(name, "profile "))
			default:
				continue // sso-session, services and other non-profile sections
			}
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}
