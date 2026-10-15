# llm-request-tollgate 常用命令入口.
# 用法: make <target>; 不带 target 默认 build. 详细操作见 docs/operations.md.

# 构建缓存放项目内 (tmp/ 已 gitignore), 不污染 ~/.cache/go-build:
GOCACHE ?= $(CURDIR)/tmp/gocache
export GOCACHE

BIN := llm-request-tollgate

.PHONY: help build vet test selftest healthcheck install fmt check

help:
	@echo "targets: build vet test selftest healthcheck install fmt check"
	@echo "  build       编译代理二进制"
	@echo "  vet         静态检查 (go vet ./...)"
	@echo "  test        单元与端到端测试 (go test ./...)"
	@echo "  selftest    全量自测 (假上游, 17 组用例, 见 test/run_selftest.sh)"
	@echo "  healthcheck 自检 (二进制/配置/端口; key_env 密钥请先 export)"
	@echo "  install     安装 systemd 用户服务 (需先有 deploy/tollgate.service)"
	@echo "  fmt         gofmt 检查 (gofmt -l .)"
	@echo "  check       提交前检查 (vet + test + fmt)"

build:
	go build -o $(BIN) .

vet:
	go vet ./...

test:
	go test ./...

selftest:
	./test/run_selftest.sh all

healthcheck:
	./test/healthcheck.example.sh

install:
	./deploy/tollgate.sh install

fmt:
	gofmt -l .

check: vet test fmt
