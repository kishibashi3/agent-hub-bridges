BINARY := bridge-claude2
CMD     := ./cmd/bridge
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build build-arm64 vet test clean install

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

install: build
	install -d $(INSTALL_DIR)
	install -m 755 $(BINARY) $(INSTALL_DIR)/$(BINARY)

# 使用例:
#   make build
#   AGENT_HUB_URL=http://localhost:3000/mcp GITHUB_PAT=ghp_... \
#     ./bridge-claude2 --user bridges-go-impl --workdir /path/to/workdir
#
#   make install                      # ~/.local/bin に bridge-claude2 を配置
#   PATH に ~/.local/bin が入っていれば以降 basename 起動が可能
