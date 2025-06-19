all:
	CGO_ENABLED=0 go build -ldflags "-s -w" -o bsync

test:
	bash ./test_edge_cases.sh