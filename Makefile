.PHONY: all build bin test vet e2e guestd sdk-test mcp-test cell-test live-netns pg-test gvisor-test bench clean

all: build guestd

# CGO_ENABLED=0 on the BUILD targets only.
#
# Nothing here needs cgo -- the sqlite driver is modernc.org/sqlite, pure Go --
# but the default CGO_ENABLED=1 links substrate-shim against the host's glibc.
# The kernel (gVisor) tier copies that binary into a TENANT-supplied rootfs,
# where a musl/distroless/scratch image has no matching loader, so the agent
# dies in ld.so and the host reports "guest agent never connected" 30s later.
#
# Deliberately NOT a top-level export: `go test -race` requires cgo.
build:
	CGO_ENABLED=0 go build ./...

# bin/ is what a node actually deploys; same reasoning as above.
bin:
	CGO_ENABLED=0 go build -o bin/substrate-shim ./cmd/shim
	CGO_ENABLED=0 go build -o bin/noded ./cmd/noded
	CGO_ENABLED=0 go build -o bin/cell ./cmd/cell
	CGO_ENABLED=0 go build -o bin/subctl ./cmd/subctl

vet:
	go vet ./...

test:
	go test ./internal/... -count=1 -timeout 300s

guestd:
	cargo build --release --manifest-path guestd/Cargo.toml

guestd-test:
	cargo test --manifest-path guestd/Cargo.toml

sdk-python:
	python3 sdk/python/test_sdk.py

sdk-typescript:
	cd sdk/typescript && npm test

sdk-go:
	cd sdk/go && go test ./...

mcp-test:
	cd mcp && npm install --no-audit --no-fund --silent && node test/test.mjs

pg-test:
	@docker rm -f substrate-pg >/dev/null 2>&1 || true
	docker run -d --name substrate-pg -e POSTGRES_PASSWORD=substrate -e POSTGRES_DB=substrate -p 55432:5432 postgres:16-alpine
	@until docker exec substrate-pg pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	SUBSTRATE_TEST_DB_URL="postgres://postgres:substrate@127.0.0.1:55432/substrate?sslmode=disable" \
		go test ./internal/store -run Postgres -count=1 -v
	@docker rm -f substrate-pg >/dev/null 2>&1 || true

gvisor-test:
	sudo -E env PATH="$$PATH" go test ./internal/vmm/gvisor -run Live -count=1 -v

live-netns:
	sudo -E env PATH="$$PATH" go test ./internal/netns -run Live -count=1 -v

cell-test:
	@docker rm -f substrate-pg-cell >/dev/null 2>&1 || true
	docker run -d --name substrate-pg-cell -e POSTGRES_PASSWORD=substrate -e POSTGRES_DB=cellint -p 55433:5432 postgres:16-alpine
	@until docker exec substrate-pg-cell pg_isready -U postgres >/dev/null 2>&1; do sleep 1; done
	SUBSTRATE_TEST_DB_URL="postgres://postgres:substrate@127.0.0.1:55433/cellint?sslmode=disable" \
		go test ./internal/cell/inttest/ -tags cell -count=1 -v
	@docker rm -f substrate-pg-cell >/dev/null 2>&1 || true

e2e:
	go test ./internal/api -tags e2e -count=1 -timeout 300s

bench:
	go build -o bin/phase0 ./bench/phase0

templates-demo:
	bash tools/rootfs/build-demo.sh tools/rootfs/demo-rootfs
	go run ./cmd/templates-build --dir tools/rootfs/demo-rootfs --out tools/rootfs/out

clean:
	rm -rf bin
	cargo clean --manifest-path guestd/Cargo.toml 2>/dev/null || true
