BRIDGE  := bridge-tmux
CTL     := agenthubctl
CMD_BRIDGE := ./cmd/bridge
CMD_CTL    := ./cmd/agenthubctl
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build build-arm64 vet test clean install

build:
	go build -o $(BRIDGE) $(CMD_BRIDGE)
	go build -o $(CTL)    $(CMD_CTL)

build-arm64:
	GOOS=linux GOARCH=arm64 go build -o $(BRIDGE)-arm64 $(CMD_BRIDGE)
	GOOS=linux GOARCH=arm64 go build -o $(CTL)-arm64    $(CMD_CTL)

vet:
	go vet ./...

test:
	go test ./...

clean:
	rm -f $(BRIDGE) $(BRIDGE)-arm64 $(CTL) $(CTL)-arm64

install: build
	install -d $(INSTALL_DIR)
	install -m 755 $(BRIDGE) $(INSTALL_DIR)/$(BRIDGE)

# 使用例:
#   make build
#   AGENT_HUB_URL=http://... GITHUB_PAT=ghp_... \
#     ./bridge-tmux --user reviewer --workdir /path/to/reviewer
#
#   ./agenthubctl --fleet fleet.yaml start
#   ./agenthubctl --fleet fleet.yaml status
#   ./agenthubctl --fleet fleet.yaml stop
#
#   make install                      # ~/.local/bin に bridge-tmux だけを配置
#   agenthubctl は配置しない。agent-hub-control の make install で入れる (#287)
#   PATH に ~/.local/bin が入っていれば、agenthubctl bridge spawn は bridge-tmux を basename で解決して起動できる
