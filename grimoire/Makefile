# Top-level build orchestration.
#
# Why a Makefile instead of just `go build`?
#   - whisper.cpp (cgo dependency at third_party/whisper.cpp) needs its
#     static libs built before `go build` can link them. cmake configures
#     and builds those libs.
#   - Real-model ASR tests need a ~75MB model file downloaded; the default
#     test suite uses the bundled <1MB structural mock so CI stays fast.

WHISPER_DIR := third_party/whisper.cpp
WHISPER_BUILD := $(WHISPER_DIR)/build
WHISPER_LIB := $(WHISPER_BUILD)/src/libwhisper.a

MODELS_DIR := $(WHISPER_DIR)/models
TINY_EN := $(MODELS_DIR)/ggml-tiny.en.bin
BASE_EN := $(MODELS_DIR)/ggml-base.en.bin

GOFLAGS ?=

.PHONY: build test test-real-asr whisper-build whisper-clean tiny-model base-model fmt vet clean

## Default: build the binary.
build: $(WHISPER_LIB)
	go build $(GOFLAGS) -o stackend ./cmd/stackend

## Fast unit tests. Uses the mock whisper model bundled with whisper.cpp.
test: $(WHISPER_LIB)
	go test ./...

## Slow tests that exercise the real whisper model. Requires `make tiny-model`.
test-real-asr: $(WHISPER_LIB) $(TINY_EN)
	WHISPER_TINY_EN_MODEL=$(abspath $(TINY_EN)) \
	  go test -tags=realasr ./internal/asr/

## Build whisper.cpp static libs. Idempotent.
whisper-build: $(WHISPER_LIB)

$(WHISPER_LIB):
	@echo "==> building whisper.cpp"
	cd $(WHISPER_DIR) && cmake -B build \
	  -DBUILD_SHARED_LIBS=OFF \
	  -DWHISPER_BUILD_TESTS=OFF \
	  -DWHISPER_BUILD_EXAMPLES=OFF \
	  -DWHISPER_BUILD_SERVER=OFF \
	  -DGGML_OPENMP=OFF
	cd $(WHISPER_DIR) && cmake --build build --config Release -j$$(nproc)

whisper-clean:
	rm -rf $(WHISPER_BUILD)

## Download the tiny English model (~75MB). Cached. Used by test-real-asr.
tiny-model: $(TINY_EN)

$(TINY_EN):
	@echo "==> downloading ggml-tiny.en (~75MB)"
	cd $(WHISPER_DIR) && bash models/download-ggml-model.sh tiny.en

## Download the base English model (~142MB). Cached. Bundled into the image
## by the Containerfile (the runtime ASR model).
base-model: $(BASE_EN)

$(BASE_EN):
	@echo "==> downloading ggml-base.en (~142MB)"
	cd $(WHISPER_DIR) && bash models/download-ggml-model.sh base.en

fmt:
	gofmt -w -s $$(find . -name '*.go' -not -path './third_party/*')

vet:
	go vet ./...

clean: whisper-clean
	rm -f stackend
