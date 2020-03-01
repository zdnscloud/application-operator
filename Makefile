VERSION=`git describe --tags`
BUILD=`date +%FT%T%z`
BRANCH=`git branch | sed -n '/\* /s///p'`

LDFLAGS=-ldflags "-w -s -X main.version=${VERSION} -X main.build=${BUILD}"
GOSRC = $(shell find . -type f -name '*.go')

REGISTRY_NAME = zdnscloud
IMAGE_NAME = application-operator
IMAGE_VERSION = latest

build:
	CGO_ENABLED=0 GOOS=linux go build cmd/application-operator/application-operator.go

build-image:
	docker build -t $(REGISTRY_NAME)/$(IMAGE_NAME):${BRANCH} --build-arg version=${VERSION} --build-arg buildtime=${BUILD} .
	docker image prune -f

docker: 
	docker build -t $(REGISTRY_NAME)/$(IMAGE_NAME):${IMAGE_VERSION} --build-arg version=${VERSION} --build-arg buildtime=${BUILD} .
	docker image prune -f
	docker push $(REGISTRY_NAME)/$(IMAGE_NAME):${IMAGE_VERSION}

clean:
	rm -f application-operator 

clean-image:
	docker rmi $(REGISTRY_NAME)/$(IMAGE_NAME):${BRANCH}

.PHONY: build
