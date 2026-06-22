BINARY  := hocker
GOARCH  ?= arm64

.PHONY: build build-linux vet fmt test rootfs clean

# Build for the host. On macOS this only type-checks; the binary will not run.
build:
	go build -o $(BINARY) .

# Build a Linux binary you can copy into a VM and run.
build-linux:
	GOOS=linux GOARCH=$(GOARCH) go build -o $(BINARY)-linux .

vet:
	go vet ./...

fmt:
	gofmt -w .

# Run the integration tests. They need Linux, root, and a cgroup v2 host. They
# download a rootfs unless HOCKER_TEST_ROOTFS points at one already, e.g.
#   make test HOCKER_TEST_ROOTFS=/var/lib/hocker/rootfs
test: build
	go test -c -o $(BINARY).test .
	sudo env "HOCKER_BIN=$(CURDIR)/$(BINARY)" "HOCKER_TEST_ROOTFS=$(HOCKER_TEST_ROOTFS)" ./$(BINARY).test -test.v -test.timeout 5m
	rm -f $(BINARY).test

# Download and unpack a minimal root filesystem into ./rootfs (run on Linux).
rootfs:
	./scripts/get-rootfs.sh

clean:
	rm -f $(BINARY) $(BINARY)-linux $(BINARY).test
