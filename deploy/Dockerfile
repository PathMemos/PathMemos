# syntax=docker/dockerfile:1

ARG ALPINE_MIRROR=mirrors.aliyun.com
ARG GOPROXY=https://goproxy.cn,https://goproxy.io,direct

FROM golang:1.25.12-alpine AS builder

# 全局 ARG 在 FROM 后失效，stage 内必须重新声明才能取到默认值/构建参数
ARG ALPINE_MIRROR
ARG GOPROXY

RUN sed -i "s/dl-cdn.alpinelinux.org/${ALPINE_MIRROR:-mirrors.aliyun.com}/g" /etc/apk/repositories
ENV GOPROXY=${GOPROXY:-https://goproxy.cn,https://goproxy.io,direct}

WORKDIR /app

RUN apk add --no-cache git ca-certificates

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /app/bin/pathmemos ./backend/cmd/server

FROM alpine:3.21

ARG ALPINE_MIRROR

RUN sed -i "s/dl-cdn.alpinelinux.org/${ALPINE_MIRROR:-mirrors.aliyun.com}/g" /etc/apk/repositories
RUN apk add --no-cache ca-certificates tzdata curl

WORKDIR /app

COPY --from=builder /app/bin/pathmemos /app/pathmemos
COPY --from=builder /app/backend/migrations /app/backend/migrations
COPY --from=builder /app/deploy/assets/invite.png /app/assets/invite-share-cover.png
COPY --from=builder /app/deploy/assets/default-cover.jpg /app/assets/default-cover.jpg
COPY --from=builder /app/deploy/assets/person_invite.png /app/assets/person_invite.png
COPY --from=builder /app/deploy/assets/wxmp_thumb.jpg /app/assets/wxmp_thumb.jpg

# 同时创建 SaaS(/opt/papafeiji)与开源版(/opt/pathmemos)的上传目录，
# 保证两种部署形态下 named volume 挂载点均归 appuser 所有。
RUN mkdir -p /opt/pathmemos/uploads /opt/papafeiji/uploads && \
    adduser -D -s /bin/sh -u 1000 appuser && \
    chown -R appuser:appuser /app /opt/pathmemos/uploads /opt/papafeiji/uploads

USER appuser

EXPOSE 8080 8081

HEALTHCHECK CMD curl -f http://localhost:8080/health/ready || exit 1

ENTRYPOINT ["/app/pathmemos"]
