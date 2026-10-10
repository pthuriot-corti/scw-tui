.PHONY: build run

build:
	go build -o scw-tui .

run: build
	./scw-tui
