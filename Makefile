VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
BIN     := bin/terraform-recovery

.PHONY: build test integration vet vulncheck demo clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/terraform-recovery

test:
	go test -race ./...

# Runs the plan/apply workflow against real Terraform with the hashicorp/random
# provider. Needs terraform on PATH and network access to download the provider;
# no cloud credentials are used.
integration:
	go test -tags integration -run TestRealTerraform -v ./internal/recovery

vet:
	go vet ./...
	gofmt -l . | (! grep .)

# Reports known vulnerabilities (Go vulnerability database) that the code
# can actually reach, including the Go standard library. The govulncheck
# version is pinned in go.mod (tool directive) and updated by Dependabot.
vulncheck:
	go tool govulncheck ./...

# The demo is read-only: it uses the bundled inventory and never runs Terraform.
demo: build
	./$(BIN) --project examples/demo/terraform --inventory examples/demo/inventory.json --dry-run

clean:
	rm -rf bin
