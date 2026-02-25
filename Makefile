# Makefile for utorr

BINARY_NAME=utorr
BUILD_DIR=builds

# OS detection for .exe extension on Windows
ifeq ($(OS),Windows_NT)
	BINARY_EXE := $(BINARY_NAME).exe
else
	BINARY_EXE := $(BINARY_NAME)
endif

.PHONY: all build clean test

all: build

build: clean
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 go build -o $(BUILD_DIR)/$(BINARY_EXE) .

clean:
	@rm -rf $(BUILD_DIR)

test:
	go test ./...
