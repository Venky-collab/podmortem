.PHONY: build test lint demo demo-down

build:
	go build -o bin/podmortem ./cmd/podmortem

test:
	go test -race ./...

lint:
	gofmt -l . && go vet ./... && helm lint deploy/helm/podmortem

demo:
	./demo/run-demo.sh

demo-down:
	./demo/run-demo.sh down
