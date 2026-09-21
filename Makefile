.PHONY: build test integration

build:
	go -C watcher build -trimpath -o ../bin/oc-tmux .

test:
	go -C watcher test -race ./...
	go -C watcher vet ./...

integration: build
	python3 tests/integration.py
