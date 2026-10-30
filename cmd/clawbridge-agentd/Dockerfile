# The agentd image: the clawbridge-agentd binary alone, for the egress
# container (cmd/clawbridge-agentd/README.md, "The egress proxy"). agentd
# creates that container from its fixed spec, which sets the command
# (`egress --listen … --policy …`), user 65534, a read-only root and no
# capabilities. agentd and its socket proxy run as host binaries, not here.
#
# Build from the repository root:
#   docker build -f cmd/clawbridge-agentd/Dockerfile -t clawbridge-agentd .
# Each release publishes it as ghcr.io/jkai-chen/clawbridge-agentd, attested;
# a deploy pulls it into the agents' rootless daemon and pins that digest
# (CLAWBRIDGE_EGRESS_IMAGE=ghcr.io/jkai-chen/clawbridge-agentd@sha256:…), or
# loads a local build and pins it by image ID (sha256:…).
#
# The build stage runs on the builder's platform and cross-compiles for the
# target, as the server's Dockerfile does, and the final stage has no RUN: the
# release's arm64 image needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine@sha256:a2012b4a1d1f9f7e9f44e0d39b805a901e856b1dd80b4e6ee4338db547050308 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/clawbridge-agentd ./cmd/clawbridge-agentd

# No shell, no package manager, no CA bundle needed: the egress proxy never
# speaks TLS itself.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/clawbridge-agentd /clawbridge-agentd
USER 65534:65534
ENTRYPOINT ["/clawbridge-agentd"]
