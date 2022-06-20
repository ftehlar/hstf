all: build docker

build:
	go build ./tools/http_server
	go build .

docker:
	docker build -t hstf/vpp -f Dockerfile.vpp .

.PHONY: docker
