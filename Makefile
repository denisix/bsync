all:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bsync
	-upx --best bsync || true

test:
	go test -v ./...
	@tmp_bin=$$(mktemp /tmp/bsync-test-bin.XXXXXX); \
	trap 'rm -f "$$tmp_bin"' EXIT; \
	CGO_ENABLED=0 go build -trimpath -o "$$tmp_bin" .; \
	BSYNC_BIN="$$tmp_bin" bash ./test_edge_cases.sh

test-fault:
	go test -count=1 -run 'Test(Fault|Strict|Sysfs|RunQuickBenchmark|WaitForReadyNoReady|ShellQuote|RemoteBinary|ValidateProtocol|BlockPayload)' ./...
	@tmp_bin=$$(mktemp /tmp/bsync-fault-bin.XXXXXX); \
	trap 'rm -f "$$tmp_bin"' EXIT; \
	CGO_ENABLED=0 go build -trimpath -o "$$tmp_bin" .; \
	BSYNC_BIN="$$tmp_bin" bash ./test_fault_cli.sh

test-stress:
	go test -race ./...
	@tmp_bin=$$(mktemp /tmp/bsync-stress-bin.XXXXXX); \
	trap 'rm -f "$$tmp_bin"' EXIT; \
	CGO_ENABLED=0 go build -trimpath -o "$$tmp_bin" .; \
	BSYNC_BIN="$$tmp_bin" bash ./test_stress.sh

# Transfer-level benchmarks (bench_test.go): real client+server in-process
# over loopback. Run before and after any performance change and compare with
# `benchstat old.txt new.txt`.
bench:
	go test -run '^$$' -bench . -benchmem -count 3

test-remote:
	@test -n "$(REMOTE)" || { echo "usage: make test-remote REMOTE=user@host"; exit 2; }
	@tmp_bin=$$(mktemp /tmp/bsync-remote-bin.XXXXXX); \
	trap 'rm -f "$$tmp_bin"' EXIT; \
	CGO_ENABLED=0 go build -trimpath -o "$$tmp_bin" .; \
	BSYNC_BIN="$$tmp_bin" REMOTE="$(REMOTE)" bash ./test_remote_ssh.sh

# Windows cross-compile (run from Linux: requires GOOS=windows)
windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bsync.exe
	-upx --best bsync.exe || true

# Release: cross-compiled binaries for linux/amd64, linux/arm64,
# windows/amd64, darwin/amd64, darwin/arm64 into dist/. Each target builds
# standalone (no shared state), so `make -j5 release` parallelizes safely.
dist:
	mkdir -p dist

release: release-linux-amd64 release-linux-arm64 release-windows-amd64 release-darwin-amd64 release-darwin-arm64

release-linux-amd64: dist
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/bsync-linux-amd64
	-upx --best dist/bsync-linux-amd64 || true

release-linux-arm64: dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/bsync-linux-arm64
	-upx --best dist/bsync-linux-arm64 || true

release-windows-amd64: dist
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/bsync-windows-amd64.exe
	-upx --best dist/bsync-windows-amd64.exe || true

# upx does not support Mach-O (macOS) binaries, so these are left uncompressed.
release-darwin-amd64: dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o dist/bsync-darwin-amd64

release-darwin-arm64: dist
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "-s -w" -o dist/bsync-darwin-arm64

clean:
	rm -f bsync bsync.exe
	rm -rf dist

.PHONY: all windows test test-fault test-stress test-remote release release-linux-amd64 release-linux-arm64 release-windows-amd64 release-darwin-amd64 release-darwin-arm64 clean
