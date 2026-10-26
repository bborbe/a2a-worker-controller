include Makefile.variables
include Makefile.precommit
include example.env

SERVICE = bborbe/a2a-worker-controller

# Included AFTER SERVICE and the variables it renders are assigned: Makefile.docker's
# guard is evaluated at parse time, so including it above this block would check empty
# values, pass, and let them through to the shell lines that interpolate them.
include Makefile.docker

run:
	@go run -mod=mod main.go \
	-sentry-dsn="$(shell teamvault-url --teamvault-config ~/.teamvault.json --teamvault-key=${SENTRY_DSN_KEY})" \
	-listen="localhost:${A2A_WORKER_CONTROLLER_PORT}" \
	-a2a-public-url="http://localhost:${A2A_WORKER_CONTROLLER_PORT}${A2A_PATH}" \
	-v=2

deps:
	go install github.com/bborbe/teamvault-utils/cmd/teamvault-config-parser@latest
	go install github.com/bborbe/teamvault-utils/cmd/teamvault-file@latest
	go install github.com/bborbe/teamvault-utils/cmd/teamvault-url@latest
	go install github.com/bborbe/teamvault-utils/cmd/teamvault-username@latest
	go install github.com/bborbe/teamvault-utils/cmd/teamvault-password@latest
	go install github.com/onsi/ginkgo/v2/ginkgo@v2.25.3
	sudo port install trivy

# Builds the local launchd binary, signs it with a stable local identity, and
# moves it into place, then enforces the launchd baseline and reloads the job.
#
# Never `cp` a new build over the installed binary. Overwriting a signed binary
# in place leaves macOS holding the old file's code-signature state, so the next
# exec is killed with `SIGKILL (Code Signature Invalid)`; under KeepAlive that
# becomes a crash loop. That is what preceded the 2026-10-17 kernel panic. Here
# the binary is built and signed at a staging path, verified, and only then
# `mv`ed over the target (a new file, not an overwrite). The fixed identity and
# identifier also keep the TCC grant across rebuilds.
#
# The package (`.`) is built, never `main.go`, so the binary keeps its VCS stamp.
# CODESIGN_IDENTITY must exist in the login keychain
# (`security find-identity -p codesigning`).
CODESIGN_IDENTITY ?= bborbe local codesign
CODESIGN_ID ?= de.bborbe.a2a-worker-controller
INSTALL_BIN ?= $(HOME)/.local/bin/a2a-worker-controller
LAUNCHD_LABEL ?= com.bborbe.a2a-worker-controller
LAUNCHD_PLIST ?= $(HOME)/Library/LaunchAgents/$(LAUNCHD_LABEL).plist

.PHONY: install
install:
	@security find-identity -p codesigning | grep -qF '"$(CODESIGN_IDENTITY)"' \
		|| { echo "codesign identity '$(CODESIGN_IDENTITY)' not in keychain; check: security find-identity -p codesigning" >&2; exit 1; }
	@test -f "$(LAUNCHD_PLIST)" || { echo "plist not installed: $(LAUNCHD_PLIST)" >&2; exit 1; }
	mkdir -p "$(dir $(INSTALL_BIN))"
	go build -o "$(INSTALL_BIN).new" .
	codesign -f -s "$(CODESIGN_IDENTITY)" -i $(CODESIGN_ID) "$(INSTALL_BIN).new"
	codesign --verify --strict "$(INSTALL_BIN).new"
	codesign -d -r- "$(INSTALL_BIN).new" 2>&1 | grep -q 'identifier "$(CODESIGN_ID)" and certificate leaf = H"'
	mv -f "$(INSTALL_BIN).new" "$(INSTALL_BIN)"
	$(MAKE) launchd-baseline
	$(MAKE) launchd-reload

# Enforces the launchd baseline on the installed plist (hand-installed; it runs
# the TeamVault wrapper ~/.local/bin/a2a-worker-controller.sh): ThrottleInterval
# 30, so a crash loop relaunches at most twice a minute instead of launchd's 10 s
# default, and NumberOfFiles 4096. Idempotent; touches only these two keys.
.PHONY: launchd-baseline
launchd-baseline:
	@test -f "$(LAUNCHD_PLIST)" || { echo "plist not installed: $(LAUNCHD_PLIST)" >&2; exit 1; }
	/usr/libexec/PlistBuddy -c 'Set :ThrottleInterval 30' "$(LAUNCHD_PLIST)" 2>/dev/null \
		|| /usr/libexec/PlistBuddy -c 'Add :ThrottleInterval integer 30' "$(LAUNCHD_PLIST)"
	/usr/libexec/PlistBuddy -c 'Set :SoftResourceLimits:NumberOfFiles 4096' "$(LAUNCHD_PLIST)" 2>/dev/null \
		|| /usr/libexec/PlistBuddy -c 'Add :SoftResourceLimits:NumberOfFiles integer 4096' "$(LAUNCHD_PLIST)"
	plutil -lint "$(LAUNCHD_PLIST)"

# bootout + bootstrap, never `kickstart -k`: kickstart keeps the plist launchd
# cached at bootstrap, so a baseline edit would not apply. bootout is
# asynchronous, so wait (max 20 s) for the job to be gone before bootstrapping,
# or bootstrap fails with `5: Input/output error`.
.PHONY: launchd-reload
launchd-reload:
	-launchctl bootout gui/$$(id -u)/$(LAUNCHD_LABEL)
	@i=0; while launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) >/dev/null 2>&1; do \
		i=$$((i+1)); [ $$i -le 40 ] || { echo "bootout did not finish" >&2; exit 1; }; sleep 0.5; done
	launchctl bootstrap gui/$$(id -u) "$(LAUNCHD_PLIST)"

formatenv:
	cat example.env | sort > c
	mv c example.env

.PHONY: fix
fix:
	@for dir in $$(find `pwd` -type d -name vendor -prune -o -name go.mod -exec dirname "{}" \; | grep -v '^$$'); do \
		cd $${dir}; \
		echo "fix $${dir}"; \
		go get github.com/go-git/go-git/v5@latest; \
		go get github.com/containerd/containerd@latest; \
		go get golang.org/x/crypto@latest; \
		go get golang.org/x/net@latest; \
	done
