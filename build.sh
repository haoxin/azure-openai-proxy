#!/bin/bash

set -e

VERSION=v3

rm -rf bin

export GOOS=linux
export GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o bin/azure-openai-proxy ./cmd

docker build -t chenghxdocker/azure-openai-proxy:$VERSION .