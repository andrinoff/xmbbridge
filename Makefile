BINARY  := bridge
PKG     := ./cmd/bridge

.PHONY: build
build:
	go build -o $(BINARY) $(PKG)

.PHONY: install
install:
	go install $(PKG)

.PHONY: run
run:
	go run $(PKG) -config config.yaml

.PHONY: check
check:
	go run $(PKG) -config config.yaml -check

.PHONY: test
test:
	go test ./...

.PHONY: test-race
test-race:
	go test -race ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: docker
docker:
	docker build -t xmbbridge:latest .

.PHONY: smoke
smoke: build
	./$(BINARY) -config config.yaml -check

.PHONY: clean
clean:
	rm -f $(BINARY)
	rm -rf dist