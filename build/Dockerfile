# syntax=docker/dockerfile:1

FROM --platform=$BUILDPLATFORM golang:1.24 AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY . .
RUN make -e GOARCH=${TARGETARCH} release

FROM ubuntu:24.04
COPY --from=builder /app/model-csi-driver /usr/bin/model-csi-driver
ENTRYPOINT ["/usr/bin/model-csi-driver"]
