# Build QQ plugin for HomeAgent
# Usage: make          # build plugin.so
#        make clean    # remove build artifacts

PLUGIN_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
SDK_ROOT := $(realpath $(PLUGIN_DIR)../..)

.PHONY: all clean

all: plugin.so

plugin.so:
	cd $(SDK_ROOT) && go build -buildmode=plugin -o $(PLUGIN_DIR)plugin.so $(PLUGIN_DIR)

clean:
	rm -f $(PLUGIN_DIR)plugin.so
