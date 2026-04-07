all:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bsync
	upx --best bsync

test:
	bash ./test_edge_cases.sh

# Windows cross-compile (run from Linux: requires GOOS=windows)
windows:
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bsync.exe

clean:
	rm -f bsync

.PHONY: all windows test clean
