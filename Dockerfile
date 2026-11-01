FROM golang:1.26.0-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -o /out/sandboxd ./cmd/sandboxd && CGO_ENABLED=0 go build -trimpath -o /out/sandbox-runner ./cmd/sandbox-runner

FROM scratch AS control
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/sandboxd /sandboxd
USER 65532:65532
ENTRYPOINT ["/sandboxd"]

FROM golang:1.26.0-bookworm AS workspace
COPY --from=build /out/sandbox-runner /usr/local/bin/sandbox-runner
RUN mkdir -p /workspace && chown 65532:65532 /workspace
ENV HOME=/workspace GOCACHE=/tmp/go-cache GOPATH=/workspace/go GOTOOLCHAIN=local
WORKDIR /workspace
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/sandbox-runner", "--listen=:8090", "--workdir=/workspace"]
