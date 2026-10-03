# The runtime contains only the static gateway, CA roots and a writable data directory.
FROM --platform=$BUILDPLATFORM golang:1.26.6-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /gateway ./cmd/gateway \
    && mkdir -p /data && chown 10001:10001 /data && chmod 700 /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /gateway /gateway
COPY --from=build --chown=10001:10001 /data /data
USER 10001:10001
EXPOSE 8080 8081
ENTRYPOINT ["/gateway"]
CMD ["serve"]
