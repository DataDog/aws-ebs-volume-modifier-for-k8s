PROTO_FILE=modify.proto
PROTO_GENERATED_FILES_PATH=pkg/rpc
VERSION="v0.1.1"
LDFLAGS="-X 'main.version=$(VERSION)'"
.PHONY: all
all: build

.PHONY: build
build:
	go build -o bin/main -ldflags ${LDFLAGS} cmd/main.go

.PHONY: proto
proto:
	protoc --go_out=$(PROTO_GENERATED_FILES_PATH) --go_opt=paths=source_relative --go-grpc_out=$(PROTO_GENERATED_FILES_PATH) --go-grpc_opt=paths=source_relative $(PROTO_FILE)

.PHONY: test
test:
	go test ./... -race

.PHONY: clean
clean:
	rm -rf bin/


.PHONY: check
check: check-proto

.PHONY: linux/$(ARCH) bin/aws-ebs-csi-driver
linux/$(ARCH): bin/aws-ebs-csi-driver
bin/aws-ebs-csi-driver: | bin
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -mod=mod -ldflags ${LDFLAGS} -o bin/aws-ebs-csi-driver ./cmd

.PHONY: check-proto
check-proto:
	$(eval TMPDIR := $(shell mktemp -d))
	protoc --go_out=$(TMPDIR) --go_opt=paths=source_relative --go-grpc_out=$(TMPDIR) --go-grpc_opt=paths=source_relative $(PROTO_FILE)
	diff -r $(TMPDIR) $(PROTO_GENERATED_FILES_PATH) || (printf "\nThe proto file seems to have been modified. PLease run `make proto`."; exit 1)
	rm -rf $(TMPDIR)
