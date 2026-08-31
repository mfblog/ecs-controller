FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG ECS_VERSION=dev
ARG ECS_COMMIT=dev
ARG ECS_BUILD_DATE=unknown
# Build for the architecture selected by the container base image. This keeps
# native ARM64 builds native on Colima while still working for amd64 builders.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X github.com/Kori1c/ecs-controller/internal/app.Version=${ECS_VERSION} -X github.com/Kori1c/ecs-controller/internal/app.Commit=${ECS_COMMIT} -X github.com/Kori1c/ecs-controller/internal/app.BuildDate=${ECS_BUILD_DATE}" -o /out/ecs-controller ./cmd/ecs-controller

FROM alpine:3.22

RUN apk add --no-cache ca-certificates tzdata wget su-exec \
    && addgroup -S -g 10001 ecs-controller \
    && adduser -S -D -H -u 10001 -G ecs-controller ecs-controller \
    && mkdir -p /var/lib/ecs-controller /app/static \
    && chown -R ecs-controller:ecs-controller /var/lib/ecs-controller /app

WORKDIR /app
COPY --from=builder /out/ecs-controller /app/ecs-controller
COPY --chown=ecs-controller:ecs-controller template.html /app/template.html
COPY --chown=ecs-controller:ecs-controller static /app/static

ENV TZ=Asia/Shanghai \
    ECS_APP_DIR=/app \
    ECS_DATA_DIR=/var/lib/ecs-controller \
    ECS_HTTP_ADDR=:8080

COPY --chown=root:root docker/entrypoint-go.sh /entrypoint-go.sh
RUN chmod 0755 /entrypoint-go.sh

EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD wget -q -O - http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/entrypoint-go.sh"]
