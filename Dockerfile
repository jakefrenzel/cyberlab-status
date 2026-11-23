# Build stage: the full Go toolchain, discarded after the build.
# Images are pinned by digest as well as tag, so a re-pushed tag can't change what
# gets built. Dependabot proposes updates to both.
FROM golang:1.27.4-trixie@sha256:f18c3d73a184fb0582828510722e723b63a93f7188d11dd223d9be6f6880e65d AS build

WORKDIR /src
# Download modules in their own layer, so code changes don't re-download them
COPY go.mod go.sum ./
RUN go mod download && go mod verify

COPY . .
# Set by the release workflow; plain builds report "dev"
ARG VERSION=dev
# Static binary (no libc needed at runtime), no local paths baked in, no debug info
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/cyberlab-status .

# Runtime stage: distroless "static" has no shell, package manager, or libc. Just CA
# certificates, timezone data, and a nonroot user (UID 65532).
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

COPY --from=build /out/cyberlab-status /cyberlab-status
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/cyberlab-status"]
