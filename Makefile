# NexentaStor CSI Driver makefile
#
# Test options to be set before run tests:
# NOCOLOR=1                # disable colors
# - TEST_K8S_IP=10.3.199.250 # e2e k8s tests

VENDOR ?= nexentastor
TOP_DIR ?= /go
OPT_DIR ?= /opt/${VENDOR}
BIN_DIR ?= ${OPT_DIR}/bin
ETC_DIR ?= ${OPT_DIR}/etc
DRIVER_NAME ?= ${VENDOR}-csi-driver-block
DRIVER_PATH ?= bin/${DRIVER_NAME}

BASE_IMAGE ?= alpine:3.12
BUILD_IMAGE ?= golang:alpine3.12

GIT_CONFIG ?= $(shell base64 -w 0 $$HOME/.gitconfig)
GIT_TOKEN ?= $(shell base64 -w 0 $$HOME/.git-credentials)
DRIVER_BRANCH = $(shell git rev-parse --abbrev-ref HEAD | sed -e "s/.*\\///")
DRIVER_TAG = $(shell git describe --tags)
DRIVER_MODULE ?= $(shell awk '/^module/{print $$NF}' go.mod)
DRIVER_VERSION ?= $(if $(subst HEAD,,${DRIVER_BRANCH}),$(DRIVER_BRANCH),$(DRIVER_TAG))
DRIVER_COMMIT ?= $(shell git rev-parse HEAD | cut -c 1-7)
DATETIME ?= $(shell date +'%F_%T')
UPPER_VERSION ?= $(shell echo $(DRIVER_VERSION) | tr '[:lower:]' '[:upper:]')
UPPER_COMMIT ?= $(shell echo $(DRIVER_COMMIT) | tr '[:lower:]' '[:upper:]')

OPEN_ISCSI_IQN ?= iqn.2005-07.com.nexenta:${DRIVER_NAME}
OPEN_ISCSI_VERSION ?= 128
OPEN_ISCSI_NAMESPACE ?= ISCSIADM_NAMESPACE_${UPPER_VERSION}_${UPPER_COMMIT}

IMAGE_NAME ?= ${DRIVER_NAME}

DOCKER_FILE = Dockerfile
DOCKER_FILE_TESTS = Dockerfile.tests
DOCKER_FILE_TEST_CSI_SANITY = Dockerfile.csi-sanity
DOCKER_FILE_PRE_RELEASE = Dockerfile.pre-release
DOCKER_IMAGE_PRE_RELEASE = nexentastor-csi-driver-block-pre-release
DOCKER_CONTAINER_PRE_RELEASE = ${DOCKER_IMAGE_PRE_RELEASE}-container
DOCKER_ARGS = --build-arg BUILD_IMAGE=${BUILD_IMAGE} \
              --build-arg BASE_IMAGE=${BASE_IMAGE} \
              --build-arg BIN_DIR=${BIN_DIR} \
              --build-arg TOP_DIR=${TOP_DIR} \
              --build-arg ETC_DIR=${ETC_DIR} \
              --build-arg DRIVER_PATH=${BIN_DIR}/${DRIVER_NAME} \
              --build-arg GIT_CONFIG=${GIT_CONFIG} \
              --build-arg GIT_TOKEN=${GIT_TOKEN} \
              --build-arg DRIVER_VERSION=${DRIVER_VERSION} \
              --build-arg DRIVER_MODULE=${DRIVER_MODULE} \
              --build-arg OPEN_ISCSI_IQN=${OPEN_ISCSI_IQN} \
              --build-arg OPEN_ISCSI_VERSION=${OPEN_ISCSI_VERSION} \
              --build-arg OPEN_ISCSI_NAMESPACE=${OPEN_ISCSI_NAMESPACE}

REGISTRY ?= nexenta
REGISTRY_LOCAL ?= 10.3.199.92:5000

GIT_BRANCH = $(shell git rev-parse --abbrev-ref HEAD | sed -e "s/.*\\///")
GIT_TAG = $(shell git describe --tags)
# use git branch as default version if not set by env variable, if HEAD is detached that use the most recent tag
VERSION ?= $(if $(subst HEAD,,${GIT_BRANCH}),$(GIT_BRANCH),$(GIT_TAG))
COMMIT ?= $(shell git rev-parse HEAD | cut -c 1-7)
DATETIME ?= $(shell date +'%F_%T')
LDFLAGS ?= \
	-X github.com/Nexenta/nexentastor-csi-driver-block/pkg/driver.Version=${VERSION} \
	-X github.com/Nexenta/nexentastor-csi-driver-block/pkg/driver.Commit=${COMMIT} \
	-X github.com/Nexenta/nexentastor-csi-driver-block/pkg/driver.DateTime=${DATETIME}

.PHONY: all
all:
	@echo "Some of commands:"
	@echo "  container-build                 - build driver container"
	@echo "  container-push-local            - push driver to local registry (${REGISTRY_DEVELOPMENT})"
	@echo "  container-push-remote           - push driver to hub.docker.com registry (${REGISTRY_PRODUCTION})"
	@echo "  test-all-local-image-container  - run all test using driver from local registry"
	@echo "  test-all-remote-image-container - run all test using driver from hub.docker.com"
	@echo "  release                         - create and publish a new release"
	@echo ""
	@make print-variables

.PHONY: print-variables
print-variables:
	@echo "Variables:"
	@echo "  VERSION:    ${VERSION}"
	@echo "  GIT_BRANCH: ${GIT_BRANCH}"
	@echo "  GIT_TAG:    ${GIT_TAG}"
	@echo "  COMMIT:     ${COMMIT}"
	@echo "Testing variables:"
	@echo "  TEST_K8S_IP: ${TEST_K8S_IP}"

.PHONY: build
build:
	env CGO_ENABLED=0 go build -o ${DRIVER_PATH} -ldflags "${LDFLAGS}" ./cmd

.PHONY: container-build
container-build:
	docker build -f ${DOCKER_FILE} -t ${DRIVER_NAME}:${DRIVER_VERSION} ${DOCKER_ARGS} .

.PHONY: container-push-local
container-push-local:
	docker build -f ${DOCKER_FILE} -t ${DRIVER_NAME}:${DRIVER_VERSION} ${DOCKER_ARGS} .
	docker tag ${DRIVER_NAME}:${DRIVER_VERSION} ${REGISTRY_LOCAL}/${DRIVER_NAME}:${DRIVER_VERSION}
	docker push ${REGISTRY_LOCAL}/${DRIVER_NAME}:${DRIVER_VERSION}

.PHONY: container-push-remote
container-push-remote:
	docker build -f ${DOCKER_FILE} -t ${DRIVER_NAME}:${DRIVER_VERSION} ${DOCKER_ARGS} .
	docker tag  ${DRIVER_NAME}:${DRIVER_VERSION} ${REGISTRY}/${DRIVER_NAME}:${DRIVER_VERSION}
	docker push ${REGISTRY}/${DRIVER_NAME}:${DRIVER_VERSION}

.PHONY: test
test: test-unit

.PHONY: test-unit
test-unit:
	go test ./tests/unit/arrays -v -count 1
	go test ./tests/unit/config -v -count 1
.PHONY: test-unit-container
test-unit-container:
	docker build -f ${DOCKER_FILE_TESTS} -t ${IMAGE_NAME}-test --build-arg VERSION=${VERSION} .
	docker run -i --rm -e NOCOLORS=${NOCOLORS} ${IMAGE_NAME}-test test-unit

# run e2e k8s tests using image from local docker registry
.PHONY: test-e2e-k8s-local-image
test-e2e-k8s-local-image: check-env-TEST_K8S_IP
	sed -e "s/image: nexenta/image: ${REGISTRY_LOCAL}/g" \
		./deploy/kubernetes/nexentastor-csi-driver-block.yaml > /tmp/nexentastor-csi-driver-block-local.yaml
	go test -timeout 20m tests/e2e/driver_test.go -v -count 1 \
		--k8sConnectionString="root@${TEST_K8S_IP}" \
		--k8sDeploymentFile="/tmp/nexentastor-csi-driver-block-local.yaml" \
		--k8sSecretFile="./_configs/driver-config-single-default.yaml"
.PHONY: test-e2e-k8s-local-image-container
test-e2e-k8s-local-image-container: check-env-TEST_K8S_IP
	docker build -f ${DOCKER_FILE_TESTS} -t ${IMAGE_NAME}-test --build-arg VERSION=${VERSION} \
	--build-arg TESTRAIL_URL=${TESTRAIL_URL} \
	--build-arg TESTRAIL_USR=${TESTRAIL_USR} \
	--build-arg TESTRAIL_PSWD=${TESTRAIL_PSWD} .
	docker run -i --rm -v ${HOME}/.ssh:/root/.ssh:ro \
		-e NOCOLORS=${NOCOLORS} -e TEST_K8S_IP=${TEST_K8S_IP} \
		${IMAGE_NAME}-test test-e2e-k8s-local-image

# run e2e k8s tests using image from hub.docker.com
.PHONY: test-e2e-k8s-remote-image
test-e2e-k8s-remote-image: check-env-TEST_K8S_IP
	go test -timeout 20m tests/e2e/driver_test.go -v -count 1 \
		--k8sConnectionString="root@${TEST_K8S_IP}" \
		--k8sDeploymentFile="../../deploy/kubernetes/nexentastor-csi-driver-block.yaml" \
		--k8sSecretFile="./_configs/driver-config-single-default.yaml"
.PHONY: test-e2e-k8s-local-image-container
test-e2e-k8s-remote-image-container: check-env-TEST_K8S_IP
	docker build -f ${DOCKER_FILE_TESTS} -t ${IMAGE_NAME}-test --build-arg VERSION=${VERSION} \
	--build-arg TESTRAIL_URL=${TESTRAIL_URL} \
	--build-arg TESTRAIL_USR=${TESTRAIL_USR} \
	--build-arg TESTRAIL_PSWD=${TESTRAIL_PSWD} .
	docker run -i --rm -v ${HOME}/.ssh:/root/.ssh:ro \
		-e NOCOLORS=${NOCOLORS} -e TEST_K8S_IP=${TEST_K8S_IP} \
		${IMAGE_NAME}-test test-e2e-k8s-remote-image

# csi-sanity tests:
# - tests make requests to actual NS, config file: ./tests/csi-sanity/*.yaml
# - create container with driver and csi-sanity (https://github.com/kubernetes-csi/csi-test)
# - run container to execute tests
# - nfs client requires running container as privileged one
.PHONY: test-csi-sanity-container
test-csi-sanity-container:
	docker build ${DOCKER_ARGS} \
		--build-arg SANITY_VERSION=v3.0.0 \
		-f ${DOCKER_FILE_TEST_CSI_SANITY} \
		-t ${DRIVER_NAME}-test-csi-sanity .
	docker run --privileged --net=host -v /dev:/dev -i -e NOCOLORS=${NOCOLORS} ${DRIVER_NAME}-test-csi-sanity

# run all tests (local registry image)
.PHONY: test-all-local-image
test-all-local-image: \
	test-unit \
	test-e2e-k8s-local-image
.PHONY: test-all-local-image-container
test-all-local-image-container: \
	test-unit-container \
	test-csi-sanity-container \
	test-e2e-k8s-local-image-container

# run all tests (hub.github.com image)
.PHONY: test-all-remote-image
test-all-remote-image: \
	test-unit \
	test-e2e-k8s-remote-image
.PHONY: test-all-remote-image-container
test-all-remote-image-container: \
	test-unit-container \
	test-csi-sanity-container \
	test-e2e-k8s-remote-image-container

.PHONY: check-env-TEST_K8S_IP
check-env-TEST_K8S_IP:
ifeq ($(strip ${TEST_K8S_IP}),)
	$(error "Error: environment variable TEST_K8S_IP is not set (e.i. 10.3.199.250)")
endif

.PHONY: release
release:
	@echo "New tag: 'v${VERSION}'\n\n \
		To change version set enviroment variable 'VERSION=X.X.X make release'.\n\n \
		Confirm that:\n \
		1. New version will be based on current '${GIT_BRANCH}' git branch\n \
		2. Driver container '${IMAGE_NAME}' will be built\n \
		3. Login to hub.docker.com will be requested\n \
		4. Driver version '${REGISTRY}/${IMAGE_NAME}:v${VERSION}' will be pushed to hub.docker.com\n \
		5. CHANGELOG.md file will be updated\n \
		6. Git tag 'v${VERSION}' will be created and pushed to the repository.\n\n \
		Are you sure? [y/N]: "
	@(read ANSWER && case "$$ANSWER" in [yY]) true;; *) false;; esac)
	docker login
	make generate-changelog
	make container-build
	make container-push-remote
	git add CHANGELOG.md
	git commit -m "release v${VERSION}"
	git push origin v${VERSION}
	git tag v${VERSION}
	git push --tags

.PHONY: generate-changelog
generate-changelog:
	@echo "Release tag: v${VERSION}\n"
	docker build -f ${DOCKER_FILE_PRE_RELEASE} -t ${DOCKER_IMAGE_PRE_RELEASE} --build-arg VERSION=v${VERSION} .
	-docker rm -f ${DOCKER_CONTAINER_PRE_RELEASE}
	docker create --name ${DOCKER_CONTAINER_PRE_RELEASE} ${DOCKER_IMAGE_PRE_RELEASE}
	docker cp \
		${DOCKER_CONTAINER_PRE_RELEASE}:/go/src/github.com/Nexenta/nexentastor-csi-driver-block/CHANGELOG.md \
		./CHANGELOG.md
	docker rm ${DOCKER_CONTAINER_PRE_RELEASE}

.PHONY: clean
clean:
	-go clean -r -x
	-rm -rf bin
