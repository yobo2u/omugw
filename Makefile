SHELL := /bin/bash
BIN := bin/omugw
GO ?= go

# 依赖全部在模块缓存里时可离线构建。CI 之外的日常开发不需要联网。
export GOFLAGS := -mod=mod

.DEFAULT_GOAL := help

.PHONY: help
help: ## 显示可用目标
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## 编译网关二进制
	$(GO) build -o $(BIN) ./cmd/omugw

.PHONY: test
test: ## 单元测试 + fixture 矩阵（离线，无需 API Key）
	$(GO) test ./...

.PHONY: test-race
test-race: ## 带竞态检测的测试
	$(GO) test -race ./...

.PHONY: cover
cover: ## 生成覆盖率报告
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: matrix
matrix: ## 降级矩阵完整性断言 + 文档同步检查
	$(GO) test ./internal/degrade/ -run 'TestPhase1IsComplete|TestDegradationMatrixDocIsCurrent' -v

.PHONY: matrix-update
matrix-update: ## 重新生成 docs/degradation-matrix.md
	$(GO) test ./internal/degrade/ -run TestDegradationMatrixDocIsCurrent -update-matrix

.PHONY: golden-update
golden-update: ## 重写 golden 文件（重写后必须人工审阅 diff）
	$(GO) test ./... -update

.PHONY: fmt
fmt: ## 格式化
	$(GO) fmt ./...

.PHONY: vet
vet: ## go vet
	$(GO) vet ./...

.PHONY: fmt-check
fmt-check: ## 检查格式（CI 用）
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "以下文件未格式化:"; echo "$$out"; exit 1; fi

.PHONY: licenses
licenses: ## 依赖许可证扫描 + SBOM 生成
	@command -v go-licenses >/dev/null 2>&1 || { \
		echo "缺少 go-licenses，安装: go install github.com/google/go-licenses@latest"; exit 1; }
	go-licenses check ./... \
		--disallowed_types=forbidden,restricted,unknown
	@command -v syft >/dev/null 2>&1 || { \
		echo "缺少 syft，安装: brew install syft 或见 https://github.com/anchore/syft"; exit 1; }
	syft . -o spdx-json=SBOM.spdx.json
	@echo "已生成 SBOM.spdx.json"

.PHONY: smoke
smoke: ## 端到端冒烟（需真实 API Key，CI 默认跳过）
	@if [ -z "$$OMUGW_SMOKE" ]; then \
		echo "冒烟测试需要真实上游凭据。设置 OMUGW_SMOKE=1 并配好各 Provider 的 Key 后重试。"; \
		exit 0; \
	fi
	$(GO) test ./tests/smoke/... -tags=smoke -v

.PHONY: check
check: fmt-check vet test matrix ## CI 的完整检查集

.PHONY: clean
clean: ## 清理构建产物
	rm -rf bin coverage.out SBOM.spdx.json
