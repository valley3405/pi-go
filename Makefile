# goagent Makefile
# 构建产物统一输出到 bin/（已被 .gitignore 忽略）

GO         ?= go
BINARY     := goagent
PKG        := goagent/cmd/goagent
BIN_DIR    := bin
DEMO       := goagent/examples/fauxdemo
GOFLAGS    := -trimpath
LDFLAGS    := -s -w
VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: all build demo test race vet fmt tidy clean install run

all: vet build

## build: 编译 CLI 到 bin/goagent
build:
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS) -X main.version=$(VERSION)" -o $(BIN_DIR)/$(BINARY) ./cmd/goagent

## demo: 构建并运行 faux 演示（无需 API key）
demo:
	$(GO) run ./examples/fauxdemo

## test: 运行全部测试
test:
	$(GO) test ./...

## race: 带 race 检测运行测试
race:
	$(GO) test -race ./...

## vet: 静态检查
vet:
	$(GO) vet ./...

## fmt: 格式化并检查
fmt:
	gofmt -l -w .

## tidy: 整理依赖
tidy:
	$(GO) mod tidy

## run: 直接运行 CLI（透传参数，如 make run ARGS="-p hi"）
run:
	$(GO) run ./cmd/goagent $(ARGS)

## install: 安装到 GOBIN
install:
	$(GO) install ./cmd/goagent

## clean: 清空 bin/
clean:
	rm -rf $(BIN_DIR)

## help: 列出全部目标
help:
	@grep -E '^## ' Makefile | sed 's/## //'
