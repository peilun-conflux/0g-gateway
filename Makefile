.PHONY: build test e2e lint

build:
	go build -o bin/gateway ./cmd/gateway

test:
	go test ./...

# 真网端到端（需 ZGS_PRIVATE_KEY；可选 ZGS_NODES / ZGS_ETH_RPC 覆盖默认测试网）
e2e:
	ZGS_E2E=1 go test ./integration/ -v -timeout 10m

lint:
	gofmt -l . && go vet ./...
