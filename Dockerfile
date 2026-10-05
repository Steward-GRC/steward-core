# syntax=docker/dockerfile:1
ARG GO_VERSION=1.26.6
FROM golang:${GO_VERSION} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# VERSION is the image tag and COMMIT the full source SHA. The go-buildinfo
# targets are ignored by the linker until that package is a dependency.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.buildDate=${BUILD_DATE} -X github.com/Bugs5382/go-buildinfo.Version=${VERSION} -X github.com/Bugs5382/go-buildinfo.Commit=${COMMIT}" \
    -o /out/server ./cmd/server

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/server /server
COPY --from=build /src/migrations /migrations
ENV MIGRATIONS_DIR=/migrations
USER nonroot:nonroot
EXPOSE 9090
ENTRYPOINT ["/server"]
