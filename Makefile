proto-gen:
	@export PATH=$(shell go env GOPATH)/bin:$$PATH && \
	buf generate --path sdmprotos/annotations.proto

build:
	go build -o $(shell go env GOPATH)/bin/sdm cmd/sdm/main.go
	go build -o $(shell go env GOPATH)/bin/protoc-gen-sdm ./cmd/protoc-gen-sdm
	