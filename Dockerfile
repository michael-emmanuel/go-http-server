# syntax=docker/dockerfile:1

# ---- Build stage: has the toolchain; never shipped ----
FROM golang:1.22-alpine AS build
WORKDIR /src

# Copy the module files first so dependency resolution is cached until they
# change. (This module has no third-party dependencies, so this layer is tiny,
# but the ordering stays correct if that changes.)
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
# CGO disabled: a fully static binary needs no libc in the final image.
# -trimpath removes local filesystem paths; -s -w strips debug tables.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/server ./cmd/server

# ---- Runtime stage: only what is required to run the binary ----
# distroless/static contains CA certificates, tzdata, and a nonroot user, with
# no shell or package manager, which shrinks the attack surface.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/server /server

ENV HTTP_ADDR=:8080
EXPOSE 8080

# The image has no shell or curl, so the binary probes itself.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/server", "healthcheck"]

USER nonroot:nonroot
ENTRYPOINT ["/server"]
