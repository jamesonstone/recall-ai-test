# Variables
APP_NAME := recallaitest
BUILD_DIR := bin/
DOCKER_IMAGE := $(APP_NAME):latest
DOCKER_SEED_IMAGE := $(APP_NAME):latest

# Targets
build:
	go build -o $(BUILD_DIR)$(APP_NAME) .
.PHONY: build

run:
	make build
	$(BUILD_DIR)$(APP_NAME)
.PHONY: run

docker-build:
	docker build -t $(DOCKER_IMAGE) .
.PHONY: docker-build

play:
	ffplay \
		-f rawvideo \
		-pixel_format bgr24 \
		-video_size 3840x2160 \
		-skip_initial_bytes 12 \
		output/video-out.raw
.PHONY: play

reset:
	rm ./output/video-out.raw
.PHONY: reset

test: reset run play
.PHONY: test
