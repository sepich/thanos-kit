DOCKER_IMAGE ?= 'sepa/thanos-kit'
VER ?= `git show -s --format=%cd-%h --date=format:%y%m%d`
OS ?= $(shell uname -s | tr '[A-Z]' '[a-z]')
ARCH ?= $(shell uname -m)

help: ## Displays help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-z0-9A-Z_-]+:.*?##/ { printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

.PHONY: deps
deps: ## Ensures fresh go.mod and go.sum
	@go mod tidy
	@go mod verify

.PHONY: build
build: ## Build binaries with version set
	@CGO_ENABLED=0 go build -ldflags "-w -s \
	-X github.com/prometheus/common/version.Version=${VER} \
	-X github.com/prometheus/common/version.Revision=`git rev-parse HEAD` \
	-X github.com/prometheus/common/version.Branch=`git rev-parse --abbrev-ref HEAD` \
	-X github.com/prometheus/common/version.BuildUser=${USER}@`hostname` \
	-X github.com/prometheus/common/version.BuildDate=`date +%Y%m%d-%H:%M:%S`"

.PHONY: docker
ifeq ($(OS)_$(ARCH), linux_x86_64)
docker: build
	@docker build -t "thanos-kit" -f ci.dockerfile .
else
docker: ## Builds 'thanos-kit' docker with no tag
	@docker build -t "thanos-kit" .
endif

.PHONY: docker-push
docker-push: docker # CI only
	@echo "Pushing ver: ${VER}"
ifneq (${DOCKER_PASSWORD},)
	@echo "${DOCKER_PASSWORD}" | docker login -u="${DOCKER_USERNAME}" --password-stdin
	@docker tag thanos-kit ${DOCKER_IMAGE}:latest && docker push ${DOCKER_IMAGE}:latest
endif
	@docker tag thanos-kit ${DOCKER_IMAGE}:${VER} && docker push ${DOCKER_IMAGE}:${VER}
