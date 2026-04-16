# Local development image — requires a pre-built binary at bin/manager.
# Build with:  make build && docker build -t firebolt-operator:local .
# See also:    make docker-build-local  (does both steps)
FROM gcr.io/distroless/static:nonroot
COPY bin/manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
