
VPP_PKG_PATH?=~/vpp/build-root

all:
	@echo Targets:
	@echo
	@echo "build   - build docker image from deb packages"
	@echo "run     - run previously build built image"
	@echo "vppctl  - spawn vppctl for configuration"
	@echo

build:
	@echo DEB source path set to ${VPP_PKG_PATH}
	cp ${VPP_PKG_PATH}/*.deb docker/dev/debs
	docker build -t hs-build docker/dev

run:
	docker run --rm --name hsvpp -d hs-build

vppctl:
	docker exec -it hsvpp vppctl

compose-build:
	cd docker/dev && docker-compose build

compose-run:
	cd docker/dev && docker-compose up -d

compose-down:
	cd docker/dev && docker-compose down
