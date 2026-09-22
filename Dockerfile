# Build stage
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bridge ./cmd/bridge

# Runtime stage
FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata
COPY --from=build /out/bridge /usr/local/bin/bridge

WORKDIR /var/lib/xmbbridge
VOLUME ["/var/lib/xmbbridge"]
EXPOSE 0

ENTRYPOINT ["bridge"]
CMD ["-config", "config.yaml"]