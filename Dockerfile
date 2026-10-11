# boeyu-proxy (POC)：MITM 请求改写演示，仅作技术验证用途
# ---- build stage ---------------------------------------------------------
FROM golang:1.22-alpine AS build

WORKDIR /src
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/boeyu-proxy .

# ---- runtime stage -------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=build /out/boeyu-proxy /app/boeyu-proxy
# bake default config into the image (seeds the named volume on first start)
# files sit at the BUILD CONTEXT ROOT, so no config/ subfolder is required
COPY config.json /config/config.json
COPY rules.json /config/rules.json

EXPOSE 9931 9932 9933 9934 8080

ENTRYPOINT ["/app/boeyu-proxy"]
CMD ["-config", "/config/config.json"]