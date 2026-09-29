# 构建上下文为仓库根目录：docker build -f tools/clusterbench/Dockerfile .
# source 阶段同时供 compose 的 contract 服务运行 go test，因此保留完整工具链与模块缓存。
FROM golang:1.27.0-alpine3.24@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS source

ENV CGO_ENABLED=0 \
    GOPROXY="https://proxy.golang.org|direct"
RUN apk add --no-cache bash

WORKDIR /src
COPY go.mod go.sum ./
COPY third_party/cpaembedded/go.mod third_party/cpaembedded/go.sum ./third_party/cpaembedded/
RUN go mod download

COPY third_party/cpaembedded ./third_party/cpaembedded
COPY internal ./internal
COPY tools ./tools
RUN go build -o /out/clusterbench ./tools/clusterbench


FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

COPY --from=source /out/clusterbench /usr/local/bin/clusterbench
USER 65534:65534
ENTRYPOINT ["clusterbench"]
