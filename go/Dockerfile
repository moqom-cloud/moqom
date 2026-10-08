# The control plane's image.
#
# Built from the repository root rather than from `go/`, because the module has a
# `replace` pointing at the Go SDK beside it and a build context that cannot see
# that directory resolves it from the network -- where it does not exist, since
# the SDK has no published tag yet.
#
#   docker build -f moqom/go/Dockerfile -t moqom-control .
#
# The context is therefore the parent of both repositories. That is unusual and
# it is the same reason go.work exists; both go away at the SDK's first release.

FROM golang:1.27-bookworm AS build

WORKDIR /src

# Dependencies first, in their own layer. Source changes far more often than
# go.mod does, and this is the difference between a rebuild that downloads the
# module cache again and one that does not.
COPY moqom/go/go.mod moqom/go/go.sum ./moqom/go/
COPY moqom-go/go.mod moqom-go/go.sum ./moqom-go/
# The security libraries, whole: small, and the module's `replace` points beside it.
COPY moqom-security/go ./moqom-security/go
RUN cd moqom/go && GOWORK=off go mod download

COPY moqom-go ./moqom-go
COPY moqom/go ./moqom/go

# GOWORK=off because the workspace names repositories that are not in this
# image; the `replace` in go.mod resolves the SDK on its own.
#
# CGO off for a static binary, which is what lets the runtime stage be distroless
# rather than a distribution with a libc in it.
#
# The version is stamped rather than derived, because a container has no git
# directory and a binary that reports "unknown" in an incident is a binary nobody
# can match to a commit.
ARG VERSION=dev
RUN cd moqom/go && \
    GOWORK=off CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/ ./cmd/moqom-control ./cmd/moqom-dbadmin

# The store's conformance suite, compiled as a binary so it can be run where the
# database is. It is skipped by `go test ./...` for want of a Postgres, and the
# Postgres it should be run against has no public address -- so the only place
# the suite means anything is a pod in this cluster, and this is how it gets
# there. `go test -c` rather than a `main`, because the contract is written
# against testing.T and a copy of it that was not would be a second suite
# claiming to be the first.
RUN cd moqom/go && \
    GOWORK=off CGO_ENABLED=0 GOOS=linux \
    go test -trimpath -c -o /out/moqom-conformance ./internal/store/postgres && \
    GOWORK=off CGO_ENABLED=0 GOOS=linux \
    go test -trimpath -c -o /out/moqom-billing-conformance ./internal/billing/postgres && \
    GOWORK=off CGO_ENABLED=0 GOOS=linux \
    go test -trimpath -c -o /out/moqom-byoc-conformance ./internal/byoc/postgres

# Distroless, and nonroot: there is no shell to exec into, which is a real
# limitation during an incident and a real reduction in what a remote code
# execution gets to do. The debug variant of this image exists for the former.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/moqom-control /moqom-control

# The database bootstrap tool rides along rather than getting an image of its
# own. It runs as a Job in this cluster, because the database has no public
# address and a pod is the nearest thing to it that can be scheduled on demand.
COPY --from=build /out/moqom-dbadmin /moqom-dbadmin

# So does the store's conformance suite, for the same reason: it has to run
# beside a database that only this network can reach.
COPY --from=build /out/moqom-conformance /moqom-conformance
COPY --from=build /out/moqom-billing-conformance /moqom-billing-conformance
COPY --from=build /out/moqom-byoc-conformance /moqom-byoc-conformance

# 8443 is gRPC, 8080 is the probes. Documentation rather than enforcement --
# EXPOSE opens nothing -- but the deployment reads these numbers back.
EXPOSE 8443 8080

USER nonroot:nonroot
ENTRYPOINT ["/moqom-control"]
