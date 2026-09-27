# syntax=docker/dockerfile:1
# Go ≥ 1.25 是硬要求，不是「越新越好」：Windows 上的零停机交接依赖
# net.TCPListener.File() 与 net.FileListener，而这两者在 Windows 上直到
# Go 1.25 才实现（1.22~1.24 的 net/file_windows.go 里是 `return nil, EWINDOWS`
# 的 TODO 桩）。用 1.22 构建出的 Windows 二进制会在交接时静默降级为
# 「无法复制监听句柄」，更新只能落到手工重启。
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 版本注入：与 .github/workflows/release.yml 同一套 -X，供网关「检查更新」比较版本。
# 不给 VERSION 时编出的是开发版（面板会明确显示，不参与版本比较），
# 而不是"假装已是最新"——静默的错误版本比明确的开发版更难排查。
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN LDFLAGS="-s -w -X workbuddy2api/internal/version.Version=${VERSION} \
  -X workbuddy2api/internal/version.Commit=${COMMIT} \
  -X workbuddy2api/internal/version.BuildTime=${BUILD_TIME}" \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="$LDFLAGS" -o /out/credit ./cmd/credit

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY login.sh signin.sh credit.sh /app/
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]