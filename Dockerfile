# 构建阶段：Go 1.25 编译静态二进制
FROM golang:1.25-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /service ./cmd/server

# 运行阶段：最小镜像 + 时区库（Go 侧已内嵌 tzdata，这里供排障使用）
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -u 10001 service
USER service
COPY --from=build /service /service
COPY contracts /contracts
COPY fixtures /fixtures
ENTRYPOINT ["/service"]
