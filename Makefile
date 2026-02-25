# Makefile for utorr

BINARY_NAME=utorr
BUILD_DIR=builds
CGO_ENABLED?=0

# OS detection for .exe extension on Windows
ifeq ($(OS),Windows_NT)
	BINARY_EXE := $(BINARY_NAME)-win64.exe
	MKDIR := if not exist $(BUILD_DIR) mkdir $(BUILD_DIR)
	RM := rd /s /q $(BUILD_DIR)
	SET_ENV := set CGO_ENABLED=$(CGO_ENABLED)&&
else
	BINARY_EXE := $(BINARY_NAME)-linux64
	MKDIR := mkdir -p $(BUILD_DIR)
	RM := rm -rf $(BUILD_DIR)
	SET_ENV := CGO_ENABLED=$(CGO_ENABLED)
endif

.PHONY: all build clean test

all: build

build: clean
	@$(MKDIR)
	$(SET_ENV) go build -ldflags="-s -w" -o $(BUILD_DIR)/$(BINARY_EXE) .

clean:
	@$(RM) 2>NUL || exit 0

test:
	go test ./...
