FROM alpine:latest

MAINTAINER Psiphon Inc. <info@psiphon.ca>
LABEL Description="Alpine Linux based Psiphon Tunnel-Core Server" Vendor="Psiphon Inc." Version="1.0"

RUN apk add --update \
      bash \
      ca-certificates \
    && rm -rf /var/cache/apk/*

RUN mkdir -p /opt/psiphon

ADD ["psiphond", "/opt/psiphon/"]

# All configuration files, Server databases, GeoIP databases, etc.
# should be made available via the `/opt/psiphon/config` volume
VOLUME ["/opt/psiphon/config", "/var/log/psiphon"]

EXPOSE 3000 3001 3002 3003 3004 3005 3006

WORKDIR /opt/psiphon/config

ENTRYPOINT ["/opt/psiphon/psiphond"]
CMD ["run"]
