VER ?= `git show -s --format=%cd-%h --date=format:%y%m%d`

help: ## Displays help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-z0-9A-Z_-]+:.*?##/ { printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

test: ## Run unit tests
	@go test ./...

build: ## Build binaries with version set
	@CGO_ENABLED=0 go build -ldflags "-w -s \
	-X github.com/prometheus/common/version.Version=${VER} \
	-X github.com/prometheus/common/version.Revision=`git rev-parse HEAD` \
	-X github.com/prometheus/common/version.Branch=`git rev-parse --abbrev-ref HEAD` \
	-X github.com/prometheus/common/version.BuildUser=${USER}@`hostname` \
	-X github.com/prometheus/common/version.BuildDate=`date +%Y%m%d-%H:%M:%S`"

docker: ## Builds 'thanos-kit' docker with no tag
	@docker build -t "thanos-kit" .
