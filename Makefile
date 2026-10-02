BIN    ?= diskmap
PREFIX ?= $(HOME)/.local
TARGET ?= .
SRC    := $(wildcard *.go) template.html go.mod

.PHONY: all build test vet fmt check-js check run install clean

all: check build

build: $(BIN)

$(BIN): $(SRC)
	go build -trimpath -ldflags "-s -w" -o $@ .

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# Syntax-check the page's JavaScript (skipped when node isn't installed).
check-js:
	@if command -v node >/dev/null; then \
		sed -n '/<script>/,/<\/script>/p' template.html | sed '1d;$$d' | sed 's/__DATA__/{}/' > .check.js && \
		node --check .check.js && rm -f .check.js && echo "template.html: JS OK"; \
	else echo "node not found, skipping check-js"; fi

check: vet test check-js
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed on:"; gofmt -l .; exit 1)

run: build
	./$(BIN) $(TARGET)

install: build
	install -d $(PREFIX)/bin
	install -m 755 $(BIN) $(PREFIX)/bin/$(BIN)

clean:
	rm -f $(BIN) .check.js
