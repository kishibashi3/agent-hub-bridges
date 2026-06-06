BINARY := bridge-tmux
CMD     := ./cmd/bridge

.PHONY: build build-arm64 vet test clean

build:
	go build -o $(BINARY) $(CMD)

build-arm64:
	GOOS=linux GOARCH=arm64 go build -o $(BINARY)-arm64 $(CMD)

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -f $(BINARY) $(BINARY)-arm64

# 使用例:
#   make build
#   AGENT_HUB_URL=http://... GITHUB_PAT=ghp_... \
#     ./bridge-tmux --user reviewer --workdir /path/to/reviewer
