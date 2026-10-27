# TenantScaler: one static binary, every component is a subcommand.
#   docker build -t tenantscaler:dev .
#   docker run --rm tenantscaler:dev version

# Build stage: static binary (no cgo), so it runs on a distroless base.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=0.1.0-docker
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/tenantscaler ./cmd/tenantscaler

# Runtime stage: no shell, no package manager, non-root user. The UID is
# numeric on purpose: with runAsNonRoot the kubelet refuses to start an image
# whose USER is a name ("nonroot"), because it cannot verify it is not root.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/tenantscaler /tenantscaler
USER 65532:65532
EXPOSE 8000 8080
ENTRYPOINT ["/tenantscaler"]
