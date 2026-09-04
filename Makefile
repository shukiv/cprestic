BIN  := bin
PKGS := ./...
# End-to-end tests spin up PostgreSQL clusters and restic repositories, which
# are too large for a small /tmp tmpfs. Keep their scratch space next to the
# repository instead.
E2E_TMPDIR := $(CURDIR)/.tmp

# Most cPanel servers are x86-64; override for an ARM host.
PLUGIN_ARCH         := amd64
RESTIC_VERSION      := v0.19.1
REST_SERVER_VERSION := v0.14.0

.PHONY: all build plugin test cover e2e vet fmt tools clean

all: fmt vet test build

build:
	go build -o $(BIN)/cprest-agent       ./cmd/agent
	go build -o $(BIN)/cprest-controller  ./cmd/controller
	go build -o $(BIN)/cprest-maintenance ./cmd/maintenance

# The WHM plugin tarball: statically linked so it runs on any cPanel server
# without matching libc versions, and stripped because it ships over ssh.
plugin:
	mkdir -p $(BIN)/cprest-plugin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(PLUGIN_ARCH) go build -trimpath -ldflags="-s -w" \
		-o $(BIN)/cprest-plugin/cprest-agent ./cmd/agent
	cp packaging/whm/cprest.cgi packaging/whm/install.sh packaging/whm/uninstall.sh $(BIN)/cprest-plugin/
	mkdir -p $(BIN)/cprest-plugin/cpanel/uapi \
		$(BIN)/cprest-plugin/cpanel/admin/Cprest $(BIN)/cprest-plugin/branding
	cp packaging/cpanel/*.php packaging/cpanel/install.json $(BIN)/cprest-plugin/cpanel/
	cp packaging/cpanel/uapi/Cprest.pm $(BIN)/cprest-plugin/cpanel/uapi/
	cp packaging/cpanel/admin/Cprest/Session.pm $(BIN)/cprest-plugin/cpanel/admin/Cprest/
	cp packaging/branding/cpr-badge.svg packaging/branding/cprestic-logo.svg \
		$(BIN)/cprest-plugin/branding/
	cp packaging/branding/png/cpr-badge-48.png $(BIN)/cprest-plugin/branding/
	chmod +x $(BIN)/cprest-plugin/install.sh $(BIN)/cprest-plugin/uninstall.sh $(BIN)/cprest-plugin/cprest.cgi
	tar -C $(BIN) -czf $(BIN)/cprest-plugin-$(PLUGIN_ARCH).tar.gz cprest-plugin
	@echo
	@echo "built $(BIN)/cprest-plugin-$(PLUGIN_ARCH).tar.gz"
	@echo "copy it to the cPanel server, then:"
	@echo "  tar xzf cprest-plugin-$(PLUGIN_ARCH).tar.gz && sudo cprest-plugin/install.sh"

# Unit and integration tests. The store suite starts a throwaway PostgreSQL
# and skips itself when none is installed, so this stays runnable anywhere.
# The end-to-end suite is behind a build tag; see the e2e target.
test:
	go test $(PKGS)

cover:
	go test -coverprofile=coverage.out $(PKGS)
	go tool cover -func=coverage.out | tail -1

# The full pipeline against real dependencies. Needs PostgreSQL, restic and
# rest-server; run "make tools" first.
e2e:
	mkdir -p $(E2E_TMPDIR)
	TMPDIR=$(E2E_TMPDIR) PATH="$(PATH):$(shell go env GOPATH)/bin" \
		go test -tags e2e ./internal/e2e/ -count=1 -timeout 30m

tools:
	go install github.com/restic/restic/cmd/restic@$(RESTIC_VERSION)
	go install github.com/restic/rest-server/cmd/rest-server@$(REST_SERVER_VERSION)

vet:
	go vet $(PKGS)
	go vet -tags e2e ./internal/e2e/

fmt:
	gofmt -l -w .

clean:
	trash $(BIN) coverage.out $(E2E_TMPDIR) 2>/dev/null || true
