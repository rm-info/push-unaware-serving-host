# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/push ./cmd/push \
 && mkdir -p /out/data

# distroless/static: CA certificates, tzdata-free (zones are embedded), non-root.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/push /usr/local/bin/push
COPY --from=build --chown=65532:65532 /out/data /data
ENV PUSH_DATA_DIR=/data \
    PUSH_LISTEN=0.0.0.0:8080
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/push"]
CMD ["serve"]
