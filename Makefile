.PHONY: build run test vet lint fmt check tidy

build:
	go build -o scw-tui .

run: build
	./scw-tui

test:
	go test -v -race ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./... --fix

fmt:
	gofmt -w .

check: fmt vet lint test
