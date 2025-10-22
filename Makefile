REPO_ROOT:=${CURDIR}
OUT_DIR=$(REPO_ROOT)/bin

# Go build settings
GO111MODULE=on
CGO_ENABLED=0
export GO111MODULE CGO_ENABLED

# Docker image settings
IMAGE_NAME?=kube-ingress
REGISTRY?=gcr.io/k8s-staging-networking
TAG?=$(shell echo "$$(date +v%Y%m%d)-$$(git describe --always --dirty)")
PLATFORMS?=linux/amd64,linux/arm64,linux/s390x

.PHONY: all build 
build: 
	@echo "Building all binaries..."
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o ./bin/kube-ingress ./cmd/

clean:
	rm -rf "$(OUT_DIR)/"

test:
	CGO_ENABLED=1 go test -short -v -race -count 1 ./...

lint:
	hack/lint.sh

update:
	go mod tidy

.PHONY: ensure-buildx
ensure-buildx:
	./hack/init-buildx.sh

# Individual image build targets (load into local docker)
image-build:
	docker buildx build . \
		--tag="${REGISTRY}/$(IMAGE_NAME):$(TAG)" \
		--load

# Individual image push targets (multi-platform)
image-push:
	docker buildx build . \
		--platform="${PLATFORMS}" \
		--tag="${REGISTRY}/$(IMAGE_NAME):$(TAG)" \
		--push

# The main release target, which pushes all images
release: images-push