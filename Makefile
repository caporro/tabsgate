.PHONY: test build run image container-test ui-test
test:
	go test -race ./...
	go vet ./...
build:
	CGO_ENABLED=0 go build -trimpath -o bin/tabsgate ./cmd/tabsgate
run:
	go run ./cmd/tabsgate
image:
	docker build -t tabsgate:local .
container-test:
	docker build --target test -t tabsgate:test .
ui-test:
	node scripts/ui_smoke.mjs
