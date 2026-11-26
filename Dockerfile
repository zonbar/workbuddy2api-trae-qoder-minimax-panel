# syntax=docker/dockerfile:1
# go.mod 要求 go 1.26.0，基础镜像必须跟上（用 1.23 会直接报 "go.mod requires go >= 1.26.0"）
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
# chromium：无头浏览器扫码登录（Trae / Qoder / 豆包 / CodeArts 的二维码由网页绘制）。
# 中文字体：登录页文案、二维码下方提示在截图里不能变方块。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash chromium font-noto-cjk \
    nss freetype harfbuzz ttf-freefont \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 容器里没有桌面客户端文件、DPAPI 密钥和可交互浏览器；隐藏相关入口，
# 面板改用凭据包导入 / 手动 Cookie / Token。本机桌面运行可显式设 PANEL_LOCAL_CLIENT=1。
ENV PANEL_LOCAL_CLIENT=0
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
COPY config.example.json /app/config.json
USER app
EXPOSE 7863
# /healthz 是「业务可服务」探针：号池为空或全占满时按设计返回 503，
# 不能当容器存活检查用（否则空号池的新部署永远显示 unhealthy）。
# 面板入口恒 200 且无需鉴权，适合做进程存活探活；外部负载均衡仍应探 /healthz。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/panel/ >/dev/null || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]