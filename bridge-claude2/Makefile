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
# ACTIVATE が未設定・空・0・false なら張り替えない。それ以外の値 (true / yes など) は
# 張り替えるつもりかどうか分からないので、何もせずに失敗する (#351)。
# REVISION=<commit> を渡すと、origin/main の HEAD がその commit でなければ止まる。ACTIVATE=1 のときは必須 (#351)。
# ACTIVATE も REVISION も、1 語でない値は make の段階で止める。REVISION は 7〜40 桁の 16 進 (小文字) 以外も止める。
# 'abc1234 --activate' のような値が script の引数に混ざって、ACTIVATE=1 なしで張り替わるのを防ぐため (#351)。
HEX_DIGITS := 0 1 2 3 4 5 6 7 8 9 a b c d e f
# $(call hex_strip,<文字列>,$(HEX_DIGITS)): 16 進の文字を消した残り (16 進だけなら空)
hex_strip = $(if $(2),$(call hex_strip,$(subst $(firstword $(2)),,$(1)),$(wordlist 2,16,$(2))),$(1))
# $(call hex_split,<文字列>,$(HEX_DIGITS)): 16 進の文字ごとに空白を入れる ($(words) で桁数を数えるため)
hex_split = $(if $(2),$(call hex_split,$(subst $(firstword $(2)),$(firstword $(2)) ,$(1)),$(wordlist 2,16,$(2))),$(1))
REVISION_VALUE = $(strip $(REVISION))
REVISION_DIGITS = $(call hex_split,$(REVISION_VALUE),$(HEX_DIGITS))
REVISION_INVALID = $(word 2,$(REVISION_VALUE))$(call hex_strip,$(REVISION_VALUE),$(HEX_DIGITS))$(if $(word 7,$(REVISION_DIGITS)),,short)$(word 41,$(REVISION_DIGITS))
deploy:
	$(if $(word 2,$(ACTIVATE))$(filter-out x1 x0 xfalse x,x$(strip $(ACTIVATE))),$(error ACTIVATE=$(ACTIVATE) は使えません。張り替えるときは ACTIVATE=1、張り替えないときは未設定か ACTIVATE=0 にしてください))
	$(if $(REVISION_VALUE),$(if $(REVISION_INVALID),$(error REVISION=$(REVISION) は使えません。7〜40 桁の 16 進 (小文字) の commit を 1 つだけ指定してください)))
	../scripts/deploy-fleet-binary.sh $(if $(filter x1,x$(strip $(ACTIVATE))),--activate) $(if $(REVISION_VALUE),--revision '$(REVISION_VALUE)')

# 使用例:
#   make build
#   AGENT_HUB_URL=http://localhost:3000/mcp GITHUB_PAT=ghp_... \
#     ./bridge-claude2 --user bridges-go-impl --workdir /path/to/workdir
#
#   make install                      # ~/.local/bin に bridge-claude2 を配置 (開発用)
#   make deploy                       # fleet 用: main を別 clone で build して ~/.agent-hub/bin に 3 bridge の実体を配置
#   make deploy REVISION=<commit>     # origin/main がその commit のときだけ配置する (違えば止まる)
#   make deploy ACTIVATE=1 REVISION=<commit>  # 上に加えて symlink ~/.agent-hub/bin/<bridge> を今回の実体に張り替える (REVISION 必須)
#   PATH に ~/.local/bin が入っていれば以降 basename 起動が可能
