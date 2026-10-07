FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o /worker .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && addgroup -g 10001 worker && adduser -D -u 10001 -G worker worker && mkdir /data && chown worker:worker /data
COPY --from=build /worker /usr/local/bin/floppy-vio-collections
USER 10001:10001
WORKDIR /data
ENTRYPOINT ["floppy-vio-collections"]
CMD ["-config", "/config/config.json"]