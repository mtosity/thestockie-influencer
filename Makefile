BINARY := influencer-job
PKG    := ./cmd/influencer-job

.PHONY: build build-linux run dry-run aggregate fmt vet tidy clean

## build: compile the binary for the host platform into ./bin
build:
	go build -o bin/$(BINARY) $(PKG)

## build-linux: cross-compile for the Xeon VPS (linux/amd64)
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/$(BINARY)-linux-amd64 $(PKG)

## run: build + run a manual pass (uses ./.env)
run: build
	./bin/$(BINARY) --mode manual

## dry-run: discover + log only, no transcription or writes
dry-run: build
	./bin/$(BINARY) --dry-run --verbose

## aggregate: recompute cross-influencer aggregate + digest only
aggregate: build
	./bin/$(BINARY) --aggregate-only

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
