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

clean:
	rm -f bsync

.PHONY: all windows test test-fault test-stress test-remote clean
