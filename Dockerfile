# Both bases are pinned by their multi-arch index digest, never a platform
# manifest's, so each platform gets its own base. Refresh them before each
# cut: scripts/check-image.sh bases --strict (docs/design/releases.md).
# The build stage runs on the builder's platform and cross-compiles, and the
# final stage has no RUN, so an arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS build
ARG TARGETOS
ARG TARGETARCH
# The release workflow stamps the tag, its commit and the commit's time,
# as scripts/build-release.sh does; a plain build keeps the defaults.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X clawbridge/internal/buildinfo.Version=$VERSION -X clawbridge/internal/buildinfo.Commit=$COMMIT -X clawbridge/internal/buildinfo.BuildTime=$BUILD_TIME" \
    -o /out/clawbridge ./cmd/clawbridge

# gcr.io/distroless/static-debian12:nonroot
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/clawbridge /clawbridge
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/clawbridge"]
