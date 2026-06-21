BINARY  := hocker
GOARCH  ?= arm64

.PHONY: build build-linux vet rootfs clean

# Build for the host. On macOS this only type-checks; the binary will not run.
build:
	go build -o $(BINARY) .

# Build a Linux binary you can copy into a VM and run.
build-linux:
	GOOS=linux GOARCH=$(GOARCH) go build -o $(BINARY)-linux .

vet:
	go vet ./...

# Download and unpack a minimal root filesystem into ./rootfs (run on Linux).
rootfs:
	./scripts/get-rootfs.sh

clean:
	rm -f $(BINARY) $(BINARY)-linux
