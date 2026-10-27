# The agentd image: the clawbridge-agentd binary alone, for the egress
# container (cmd/clawbridge-agentd/README.md, "The egress proxy"). agentd
# creates that container from its fixed spec, which sets the command
# (`egress --listen … --policy …`), user 65534, a read-only root and no
# capabilities. agentd and its socket proxy run as host binaries, not here.
#
# Build from the repository root:
#   docker build -f cmd/clawbridge-agentd/Dockerfile -t clawbridge-agentd .
# The deploy loads it into the agents' rootless daemon and pins it by image
# ID (CLAWBRIDGE_EGRESS_IMAGE=sha256:…).
FROM golang:1.26.9-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/clawbridge-agentd ./cmd/clawbridge-agentd

# No shell, no package manager, no CA bundle needed: the egress proxy never
# speaks TLS itself.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/clawbridge-agentd /clawbridge-agentd
USER 65534:65534
ENTRYPOINT ["/clawbridge-agentd"]
