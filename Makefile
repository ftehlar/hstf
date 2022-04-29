
VPP_PKG_PATH?=~/vpp/build-root

all:
	@echo help

build:
	go build .

build-docker:
	@echo DEB source path set to ${VPP_PKG_PATH}
	rm docker/dev/debs/*
	cp ${VPP_PKG_PATH}/*.deb docker/dev/debs
	cd docker/dev && docker build -t vpp1 -f Dockerfile.vpp1 .
	cd docker/dev && docker build -t vpp2 -f Dockerfile.vpp2 .

.PHONY: run
run:
	docker run --rm --name run_vpp1 -d vpp1
	docker run --rm --name run_vpp2 -d vpp2

vppctl:
	docker exec -it hsvpp vppctl
