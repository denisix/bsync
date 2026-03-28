all:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bsync
	upx --best bsync

test:
	bash ./test_edge_cases.sh
