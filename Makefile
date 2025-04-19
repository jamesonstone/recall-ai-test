SHELL := /usr/bin/env bash

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

score:
	@echo "Running performance test..."
	@echo "----------------------------"
	@echo "Building the project first"
	go build -o $(BUILD_DIR)$(APP_NAME) .

	@echo "Running performance benchmark..."
	@if which gtime > /dev/null 2>&1; then \
		gtime -v ./$(BUILD_DIR)$(APP_NAME) > /tmp/perf_metrics.stdout 2> /tmp/perf_metrics.txt; \
	else \
		/usr/bin/time -l ./$(BUILD_DIR)$(APP_NAME) > /tmp/perf_metrics.stdout 2> /tmp/perf_metrics.txt; \
	fi

	@echo "Calculating efficiency score..."
	@echo "----------------------------"
	@# Hard-code values - tested on a c7i.2xlarge EC2 instance
	@frames=600; \
	user_time=`grep "User time (seconds):" /tmp/perf_metrics.txt | awk '{print $$4}'`; \
	sys_time=`grep "System time (seconds):" /tmp/perf_metrics.txt | awk '{print $$4}'`; \
	peak_mem=`grep "Maximum resident set size" /tmp/perf_metrics.txt | awk '{print $$6 / 1024}'`; \
	echo "User time: $$user_time seconds"; \
	echo "System time: $$sys_time seconds"; \
	total_time=`echo "$$user_time" "$$sys_time" | awk '{print $$1 + $$2}'`; \
	echo "Total processing time: $$total_time seconds"; \
	fps=`echo "$$frames" "$$total_time" | awk '{printf "%.2f", $$1 / $$2}'`; \
	echo "Average throughput: $$fps FPS"; \
	echo "Peak memory usage: $$peak_mem MB"; \
	efficiency=`echo "$$fps" "$$peak_mem" | awk '{printf "%.5f", $$1 / $$2}'`; \
	echo ""; \
	echo "Efficiency Score = $$fps / $$peak_mem = $$efficiency"
.PHONY: score
