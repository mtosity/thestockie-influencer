BINARY := influencer-job
PKG    := ./cmd/influencer-job

.PHONY: build build-linux run dry-run aggregate 13f 13f-dry fmt vet tidy clean

## build: compile both binaries for the host platform into ./bin
build:
	go build -o bin/$(BINARY) $(PKG)
	go build -o bin/superinvestor-job ./cmd/superinvestor-job

## build-linux: cross-compile both binaries for the Xeon VPS (linux/amd64)
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/$(BINARY)-linux-amd64 $(PKG)
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "-s -w" -o bin/superinvestor-job-linux-amd64 ./cmd/superinvestor-job

## run: build + run a manual pass (uses ./.env)
run: build
	./bin/$(BINARY) --mode manual

## dry-run: discover + log only, no transcription or writes
dry-run: build
	./bin/$(BINARY) --dry-run --verbose

## aggregate: recompute cross-influencer aggregate + digest only
aggregate: build
	./bin/$(BINARY) --aggregate-only

## 13f: scan SEC 13F filings + push to Convex (skips investors with no new filing)
13f: build
	./bin/superinvestor-job

## 13f-dry: fetch + diff + print one investor, no writes (CIK=... make 13f-dry)
13f-dry: build
	./bin/superinvestor-job --investor $(CIK) --dry-run

fmt:
	gofmt -w .

vet:
	go vet ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
