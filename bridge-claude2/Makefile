BINARY := bridge-claude2
CMD     := ./cmd/bridge
INSTALL_DIR ?= $(HOME)/.local/bin

.PHONY: build build-arm64 vet test clean install deploy

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

# fleet 用の binary を配置する (issue #285 / #349)。作業ディレクトリの checkout や build 結果は使わず、
# 別 clone で main を build して、bridge-claude2 / bridge-codex2 / bridge-tmux の 3 つを
# ~/.agent-hub/bin/<bridge>@<revision> に置く。ACTIVATE=1 のときだけ symlink ~/.agent-hub/bin/<bridge> を張り替える。
deploy:
	../scripts/deploy-fleet-binary.sh $(if $(ACTIVATE),--activate)

# 使用例:
#   make build
#   AGENT_HUB_URL=http://localhost:3000/mcp GITHUB_PAT=ghp_... \
#     ./bridge-claude2 --user bridges-go-impl --workdir /path/to/workdir
#
#   make install                      # ~/.local/bin に bridge-claude2 を配置 (開発用)
#   make deploy                       # fleet 用: main を別 clone で build して ~/.agent-hub/bin に 3 bridge の実体を配置
#   make deploy ACTIVATE=1            # 上に加えて symlink ~/.agent-hub/bin/<bridge> を今回の実体に張り替える
#   PATH に ~/.local/bin が入っていれば以降 basename 起動が可能
