FROM golang:1.24-alpine AS builder

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_TIME=unknown

WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X github.com/zhaeng/zabbix-exporter/cmd/server/initial.Version=${VERSION} -X github.com/zhaeng/zabbix-exporter/cmd/server/initial.GitCommit=${GIT_COMMIT} -X github.com/zhaeng/zabbix-exporter/cmd/server/initial.BuildTime=${BUILD_TIME}" \
    -o /out/zabbix-exporter ./cmd/server

FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/zabbix-exporter /usr/local/bin/zabbix-exporter
USER 65532:65532
EXPOSE 9110
ENTRYPOINT ["/usr/local/bin/zabbix-exporter"]
CMD ["-c", "/etc/zabbix-exporter/config.yaml"]
