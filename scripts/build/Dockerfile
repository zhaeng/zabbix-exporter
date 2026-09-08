FROM registry.cn-beijing.aliyuncs.com/fanyuanhang/alpine:latest
MAINTAINER zhufuyi "g.zhufuyi@gmail.com"


# set apk repositories
RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.ustc.edu.cn/g' /etc/apk/repositories
RUN apk update

# set the time zone to Shanghai
RUN apk add tzdata  \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone \
    && apk del tzdata

# add curl, used for http service checking, can be installed without it if deployed in k8s
RUN apk add curl

# http port
EXPOSE 8080

COPY _output/zabbix-exporter /app/zabbix-exporter
COPY config.yaml /app/configs/zabbix-exporter.yml

WORKDIR /app

CMD ["./zabbix-exporter", "-c", "configs/config.yaml"]
# if you use the Configuration Center, zabbix-exporter.yml is changed to the Configuration Center configuration.
#CMD ["./zabbix-exporter", "-c", "configs/zabbix-exporter.yml", "-enable-cc"]
