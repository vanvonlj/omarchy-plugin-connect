VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
PREFIX  ?= $(HOME)/.local
BINDIR  := $(PREFIX)/bin
UNITDIR := $(HOME)/.config/systemd/user

LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet validate install uninstall clean

all: build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/omarchy-connect ./cmd/omarchy-connect

test:
	go test ./...

vet:
	go vet ./...

# The plugin half. Run before any commit that touches manifest.json or QML --
# the shell silently ignores a plugin it cannot validate.
validate:
	@command -v omarchy-plugin-validate >/dev/null 2>&1 \
		&& omarchy-plugin-validate . \
		|| echo "omarchy-plugin-validate not found; skipping (not on an Omarchy machine?)"

# Installs to the user prefix, never system-wide: the daemon runs as a systemd
# *user* unit so it inherits the Tailscale operator grant without needing root.
install: build
	install -Dm755 bin/omarchy-connect $(BINDIR)/omarchy-connect
	install -Dm644 packaging/omarchy-connect.service $(UNITDIR)/omarchy-connect.service
	systemctl --user daemon-reload
	@echo
	@echo "Installed. Start it with:"
	@echo "    systemctl --user enable --now omarchy-connect"
	@echo
	@$(BINDIR)/omarchy-connect status || true

uninstall:
	-systemctl --user disable --now omarchy-connect
	rm -f $(BINDIR)/omarchy-connect $(UNITDIR)/omarchy-connect.service
	systemctl --user daemon-reload

clean:
	rm -rf bin
